package api

import (
	"net/url"
	"strconv"
)

// Job-and-runs jobs backing `aq job`, `aq run`, and `aq
// runs`. An job is a stable, callable address in front of ONE setup
// version — creating one is handing out a GPU budget (MaxInstances +
// SpendCapCents), which is why the CLI requires both rather than defaulting
// either to unbounded.
//
// Unlike setups.go's snake_case DTOs, these routes speak camelCase on the
// wire — match the field names exactly (versionId, spendCapCents, ...), do
// not "normalize" them to snake_case.

// Job mirrors one row of GET /jobs.
type Job struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	VersionID        int    `json:"versionId"`
	Status           string `json:"status"`
	SpentCents       int64  `json:"spentCents"`
	SpendCapCents    int64  `json:"spendCapCents"`
	RunningInstances int    `json:"runningInstances"`
	RunsThisPeriod   int    `json:"runsThisPeriod"`
}

// ListJobs returns every job the caller owns.
func (c *Client) ListJobs() ([]Job, error) {
	var out []Job
	if err := c.getJSON("/jobs", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateJobRequest is the body of POST /jobs. MaxInstances and
// SpendCapCents are both always sent — the CLI never lets either be omitted
// (see jobCreate's validation), so there is no unbounded-by-default
// path on the wire either.
//
// PinnedDeploymentID pins the job to a box the customer already owns
// (attached via `aq host add` + `aq attach`) instead of hardware Aquanode
// rents. It carries `omitempty` deliberately: the zero value must never
// reach the wire as a present-but-empty key, only as an absent one, the
// server reads an absent key as "today's managed behaviour" and a present
// zero/negative one as a malformed pin. jobCreate resolves this from a
// `--on <alias>` flag locally and refuses before ever building this request
// unless the alias names a genuinely attached deployment.
type CreateJobRequest struct {
	Name               string `json:"name"`
	VersionID          int    `json:"versionId"`
	MaxInstances       int    `json:"maxInstances"`
	SpendCapCents      int64  `json:"spendCapCents"`
	PinnedDeploymentID int    `json:"pinnedDeploymentId,omitempty"`
}

// CreateJob makes a setup version callable, returning the created
// job row.
func (c *Client) CreateJob(req CreateJobRequest) (*Job, error) {
	var out Job
	if err := c.postJSON("/jobs", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RepointJobRequest is the body of POST /jobs/:id/repoint.
type RepointJobRequest struct {
	VersionID int `json:"versionId"`
}

// RepointJob switches a job to a different version — the same
// run rolls it forward or back, it just depends which VersionID is passed.
func (c *Client) RepointJob(jobID string, req RepointJobRequest) (*Job, error) {
	var out Job
	path := "/jobs/" + url.PathEscape(jobID) + "/repoint"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteJob removes a job — DELETE /jobs/:id.
func (c *Client) DeleteJob(jobID string) error {
	path := "/jobs/" + url.PathEscape(jobID)
	return c.deleteJSON(path, nil)
}

// Run mirrors one row of GET /jobs/:id/runs, and the object returned
// by GET /jobs/:id/runs/:runId.
//
// Status is one of queued|running|succeeded|failed|unservable. "unservable"
// must never be conflated with "failed": it means Aquanode could not get the
// caller a box at all (capacity, budget, or provider failure) — the workload
// itself never ran — while "failed" means it ran and the workload errored.
// Reason carries the detail for both, but especially matters for
// unservable, which is otherwise indistinguishable from a normal decline.
type Run struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	AcceptedAt string `json:"acceptedAt"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	Phase      string `json:"phase"`
}

// ListRuns returns a job's recent runs — GET /jobs/:id/runs.
func (c *Client) ListRuns(jobID string) ([]Run, error) {
	var out []Run
	path := "/jobs/" + url.PathEscape(jobID) + "/runs"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateRunRequest is the body of POST /jobs/:id/runs. Inputs is
// always a non-nil map (possibly empty) — the CLI sends `{"inputs":{}}`
// rather than omitting the field when the caller passes no --input file.
// Wait and WaitSeconds are optional; when set, the server will try to return
// a 200 with the full run object if it completes within the window.
type CreateRunRequest struct {
	Inputs      map[string]any `json:"inputs"`
	Wait        bool           `json:"wait,omitempty"`
	WaitSeconds int            `json:"waitSeconds,omitempty"`
}

// CreateRunResult is the data returned by POST /jobs/:id/runs (202 async response).
type CreateRunResult struct {
	RunID      string `json:"runId"`
	Status     string `json:"status"`
	AcceptedAt string `json:"acceptedAt"`
}

// CreateRunResponse can be either a 202 CreateRunResult or a 200 Run object.
// It merges all possible fields; the presence of certain fields indicates which
// response type was returned (RunID → 202 async, ID → 200 completed).
type CreateRunResponse struct {
	// 202 async response fields
	RunID      string `json:"runId"`
	AcceptedAt string `json:"acceptedAt"`
	// 200 sync response fields (full Run object)
	ID         string `json:"id"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	OutputRef  string `json:"outputRef"`
	// Both responses include Status and Reason
	Status string `json:"status"`
	Reason string `json:"reason"`
	Phase  string `json:"phase"`
}

// IsAsync returns true if this is a 202 async response (still queued/running).
func (r *CreateRunResponse) IsAsync() bool {
	return r.RunID != "" && r.ID == ""
}

// RunID returns either the 202 runId or the 200 id, whichever is present.
func (r *CreateRunResponse) GetRunID() string {
	if r.RunID != "" {
		return r.RunID
	}
	return r.ID
}

// CreateRun makes a run against a job. The response can be either
// 202 (async, still running) or 200 (sync, completed within the wait window).
// Use resp.IsAsync() to distinguish them, or resp.GetRunID() to get the id
// in either case.
func (c *Client) CreateRun(jobID string, req CreateRunRequest) (*CreateRunResponse, error) {
	var out CreateRunResponse
	path := "/jobs/" + url.PathEscape(jobID) + "/runs"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRun fetches one run by id — GET /jobs/:id/runs/:runId.
func (c *Client) GetRun(jobID, runID string) (*Run, error) {
	var out Run
	path := "/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RunLogChunk is one slice of a run's log, served by byte offset.
//
// `NextOffset` comes from the SERVER, never computed by advancing the local
// cursor by len(Chunk): a capped read would make the follower skip bytes it
// never saw. `Source` is a real answer and not decoration — `unreachable` means
// we could not read the log at all, which must never be printed as an empty
// log, since an empty tail reads as "nothing is being written".
type RunLogChunk struct {
	Chunk          string `json:"chunk"`
	NextOffset     int64  `json:"nextOffset"`
	Size           int64  `json:"size"`
	Truncated      bool   `json:"truncated"`
	AttemptOrdinal *int   `json:"attemptOrdinal"`
	Source         string `json:"source"`
	LogRef         string `json:"logRef,omitempty"`
}

// GetRunLogs tails a run's log from a byte offset. `attempt` of 0 means the
// latest attempt — a run that failed over has several, and concatenating them
// would produce a log whose timestamps go backwards in the middle.
func (c *Client) GetRunLogs(jobID, runID string, offset int64, attempt int) (*RunLogChunk, error) {
	var out RunLogChunk
	q := url.Values{}
	q.Set("offset", itoa64(offset))
	if attempt > 0 {
		q.Set("attempt", itoa(attempt))
	}
	path := "/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/logs?" + q.Encode()
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelRun asks for a run to stop. Cancelling an already-finished run is a
// no-op success server-side, so the CLI does not have to special-case a race it
// cannot see.
func (c *Client) CancelRun(jobID, runID string) (*Run, error) {
	var out Run
	path := "/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/cancel"
	if err := c.postJSON(path, struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func itoa(i int) string     { return strconv.Itoa(i) }
func itoa64(i int64) string { return strconv.FormatInt(i, 10) }
