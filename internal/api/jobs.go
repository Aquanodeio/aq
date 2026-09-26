package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Job-and-runs jobs backing `aq job`, `aq run`, and `aq
// runs`. An job is a stable, callable address in front of ONE setup
// version — creating one is handing out a GPU budget (MaxInstances +
// MaxInstances), which is why the CLI requires it rather than defaulting to
// unbounded. The old per-job dollar cap is gone: it could not be translated
// into runs, so it was never a control the owner could reason about.
//
// Unlike setups.go's snake_case DTOs, these routes speak camelCase on the
// wire — match the field names exactly (versionId, spendCapCents, ...), do
// not "normalize" them to snake_case.

// Job mirrors one row of GET /jobs.
type Job struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	VersionID            int    `json:"versionId"`
	Status               string `json:"status"`
	SpentCents           int64  `json:"spentCents"`
	MonthlySpendCapCents *int64 `json:"monthlySpendCapCents"`
	RunningInstances     int    `json:"runningInstances"`
	MaxInstances         int    `json:"maxInstances"`
	RunsThisPeriod       int    `json:"runsThisPeriod"`
	// Shape is DERIVED server-side from entrypoint.kind, never stored
	// (job.service.ts jobShapeFor): "batch" for a command entrypoint,
	// "service" for http/comfyui. It is what `?shape=` filters on and what
	// `aq endpoint` vs `aq job` mean by the split.
	Shape string `json:"shape"`
	// RunURL is this orchestrator's own `/api/v1/run/:jobId` route (still
	// gated by an `x-job-token`), never the box's own address. Null when
	// the server has no public base configured (job.service.ts runUrlFor) —
	// three-state, not "" — so `aq endpoint url` can tell "not configured"
	// from a row that genuinely has no id.
	RunURL *string `json:"runUrl"`
}

// ListJobs returns every job the caller owns, of every shape.
func (c *Client) ListJobs() ([]Job, error) {
	var out []Job
	if err := c.getJSON("/jobs", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListJobsByShape returns only the caller's jobs of one shape ("batch" or
// "service") — GET /jobs?shape=. Used by `aq endpoint list`, which must
// never show a batch job: the two read as unrelated concepts to a caller
// (a job you run vs. a URL you call), and mixing them on one page is the
// exact confusion the shape split exists to avoid.
func (c *Client) ListJobsByShape(shape string) ([]Job, error) {
	var out []Job
	q := url.Values{}
	q.Set("shape", shape)
	if err := c.getJSON("/jobs?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateJobRequest is the body of POST /jobs. MaxInstances and
// MaxInstances is always sent — the CLI never lets it be omitted
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
//
// VersionID and Image are the job's SOURCE XOR (job.service.ts:618-632):
// exactly one is sent, never both, never neither. VersionID carries
// `omitempty` for exactly that reason: an image-source create leaves it at
// its zero value in Go, and without `omitempty` that would post
// `"versionId":0` and trip the backend's both-or-neither check, which reads
// 0 as "sent" rather than "absent". A version row id is never legitimately
// 0, so `omitempty` is safe on the version-source path too.
type CreateJobRequest struct {
	Name         string        `json:"name"`
	VersionID    int           `json:"versionId,omitempty"`
	Image        *ImageSource  `json:"image,omitempty"`
	Entrypoint   *Entrypoint   `json:"entrypoint,omitempty"`
	Hardware     *Hardware     `json:"hardware,omitempty"`
	Placement    *JobPlacement `json:"placement,omitempty"`
	MaxInstances int           `json:"maxInstances"`
	// Pointer + omitempty: optional means the key is ABSENT on the wire, never
	// present-as-0. A zero budget would refuse every run.
	MonthlySpendCapCents *int64 `json:"monthlySpendCapCents,omitempty"`
	PinnedDeploymentID   int    `json:"pinnedDeploymentId,omitempty"`
	// Secrets names `type: "env"` team secrets (POST /secrets/teams/:teamId,
	// see internal/api/secrets.go) this job's Runs need injected at dispatch.
	// omitempty: absent means "none", the same convention every optional
	// field on this request already follows; never sent as an empty array.
	// A name with no matching live secret on the team is refused (400).
	Secrets []string `json:"secrets,omitempty"`
	// Checkpoint names the paths ogre snapshots so a reclaimed or
	// price-hopped run can resume (job.service.ts's checkpointRequired,
	// hardware.ts:99). Pointer + omitempty: the key is ABSENT on the wire
	// when the caller passes neither --checkpoint-path nor
	// --checkpoint-exclude, and the server's own refusal fires with its own
	// message. Deliberately no local required-check here: the server owns
	// this rule and is moving it to a defaulted value, so a local mirror
	// would be a second copy of a rule already scheduled for deletion.
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
}

// Checkpoint is the `{ paths, exclude? }` shape job.service.ts stores
// verbatim (the field is typed `unknown` server-side; only Paths is ever
// read, by hasCheckpointPaths). Paths carries no `omitempty`: once
// Checkpoint itself is present, the key must still read as present so the
// server's own "checkpoint.paths is required" refusal fires on an empty
// list, never silently on an absent key. Exclude keeps `omitempty` since
// most jobs need no exclusions and the console sends none when that field
// is blank.
type Checkpoint struct {
	Paths   []string `json:"paths"`
	Exclude []string `json:"exclude,omitempty"`
}

// ImageSource is the `{ ref, registrySecret? }` shape
// job.service.ts's normalizeImageSource accepts. RegistrySecret is a NAME
// resolved server-side against the team's `type: registry` secrets
// (missingSecretNames), never a token typed on the command line. `aq secret
// set --type registry` already mints and stores those. `omitempty` because a
// public image sends no key at all, matching the console's own spread.
type ImageSource struct {
	Ref            string `json:"ref"`
	RegistrySecret string `json:"registrySecret,omitempty"`
}

// Entrypoint is a `kind: "command"` entrypoint, the only kind this CLI can
// express. `http`/`comfyui` entrypoints carry a port, a body template or a
// whole workflow_api.json graph with no CLI-typeable shape, so `aq job
// create` only ever emits `command`. Argv comes verbatim from everything
// after a bare `--` on the command line (see splitRemoteCommand), never
// shell-parsed, matching the "no quoting gymnastics" the ticket asked for.
// OutputPath is required and must be absolute: entrypoint.go's
// parseEntrypoint refuses a relative or empty one outright.
type Entrypoint struct {
	Kind       string   `json:"kind"`
	Argv       []string `json:"argv"`
	OutputPath string   `json:"outputPath"`
}

// Hardware constraints for an image-source Job. See the orchestrator's
// hardware.ts HardwareSchema. GPUCount is one of the closed set 1, 2, 4, 8
// (job.go's validateJobGPUCount), matched EXACTLY against a node's own GPU
// count by job-placement.ts, never `aq up`'s free-form --gpus request.
// GPUModels are exact marketplace names (see `aq gpus`), never `aq up`'s
// substring --gpu match.
type Hardware struct {
	GPUModels []string `json:"gpuModels"`
	GPUCount  int      `json:"gpuCount"`
	DiskGB    int      `json:"diskGb"`
}

// JobPlacement is a Job's placement preferences. See the orchestrator's
// hardware.ts PlacementSchema. Named JobPlacement, not Placement, because
// control.go's Placement already names an unrelated concept (where a resumed
// deployment landed). GPUOrder carries `omitempty`: an absent key already
// means "cheapest" (today's behaviour), so that value is never written
// explicitly, exactly as the console omits it.
type JobPlacement struct {
	GPUOrder string `json:"gpuOrder,omitempty"`
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

// HttpEntrypoint is a `kind: "http"` entrypoint — a generic HTTP app, called
// by POSTing a rendered body to a port inside the box (orchestrator
// entrypoint.ts's HttpEntrypoint). `aq endpoint create` only ever emits
// Method "POST" and ResultMode "inline": the other combinations (GET/PUT,
// resultMode "poll" + resultFrom, bodyTemplate) exist on the wire but have
// no CLI-typeable shape yet, mirroring why Entrypoint above only ever emits
// `kind: "command"` — there is no console-parity flag set for them either.
type HttpEntrypoint struct {
	Kind       string `json:"kind"`
	Port       int    `json:"port"`
	Path       string `json:"path"`
	Method     string `json:"method"`
	ResultMode string `json:"resultMode"`
}

// CreateEndpointRequest is the body of POST /jobs for `aq endpoint create` —
// the service-shaped sibling of CreateJobRequest. A deliberately separate
// type rather than reusing CreateJobRequest's `Entrypoint *Entrypoint`
// field: that field's concrete type only ever carries a command entrypoint
// (argv, outputPath — neither means anything on an http entrypoint), so
// widening it to an interface would turn every existing command-create
// caller's `.Entrypoint.Argv` access into a type assertion for no benefit,
// since job and endpoint creates never share a request body in practice
// (mirrors the console's own separate /jobs/new and /endpoints/new pages
// and separate CreateJobParams-shaped bodies).
//
// MinInstances carries `omitempty`: absent means "scales to zero between
// calls" (today's default), and `aq endpoint create` only ever sends it as
// the literal 1, iff --keep-warm — never a bare 0, which would be
// indistinguishable from "not set" on the wire and is why this is a plain
// int rather than a pointer.
type CreateEndpointRequest struct {
	Name         string          `json:"name"`
	Image        *ImageSource    `json:"image,omitempty"`
	Entrypoint   *HttpEntrypoint `json:"entrypoint,omitempty"`
	Hardware     *Hardware       `json:"hardware,omitempty"`
	MaxInstances int             `json:"maxInstances"`
	MinInstances int             `json:"minInstances,omitempty"`
}

// CreateEndpoint makes an image callable over HTTP, returning the created
// job row (shape "service").
func (c *Client) CreateEndpoint(req CreateEndpointRequest) (*Job, error) {
	var out Job
	if err := c.postJSON("/jobs", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HardwareAvailabilityModel is one element of GET /jobs/hardware-availability's
// `models` array: the full GPU model UNIVERSE the marketplace can currently
// offer, independent of any gpuModels filter on the request.
type HardwareAvailabilityModel struct {
	// GPUModel is spelled exactly as the marketplace feed reports it; this
	// exact string is what goes back out as a `hardware.gpuModels` entry.
	GPUModel string `json:"gpuModel"`
}

// HardwareAvailability is what GET /jobs/hardware-availability returns.
// `--any-gpu` only needs the model universe, so this omits Offers/TotalOffers
// (they scope to a gpuModels filter this call never sends).
type HardwareAvailability struct {
	Models []HardwareAvailabilityModel `json:"models"`
}

// HardwareAvailability fetches the GPU model universe the marketplace can
// currently serve for a disk-size constraint: GET /jobs/hardware-availability
// (team-scoped, unlike the public /marketplace feed `aq gpus` reads).
//
// Used only by `aq job create --image ... --any-gpu`, the explicit opt-in
// that mirrors the console's "no card picked means any card" default
// (console/app/jobs/new/page.tsx:510-516): fetch every model the market has
// right now and send all of them, rather than leaving hardware.gpuModels
// empty, which placement refuses outright with no_gpu_models.
func (c *Client) HardwareAvailability(diskGB int) (*HardwareAvailability, error) {
	var out HardwareAvailability
	q := url.Values{}
	q.Set("diskGb", itoa(diskGB))
	path := "/jobs/hardware-availability?" + q.Encode()
	if err := c.getJSON(path, &out); err != nil {
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

// NewRunLogsStreamRequest builds (but does not send) the GET request for the
// run-log SSE stream, sending the same x-api-key/x-team-id headers GetRunLogs
// sends. It only builds the request: reading an SSE body a frame at a time
// isn't something the JSON-envelope helpers in device.go (do/getJSON/...)
// know how to do, so the caller drives resp.Body itself.
func (c *Client) NewRunLogsStreamRequest(ctx context.Context, jobID, runID string, offset int64, attempt int) (*http.Request, error) {
	q := url.Values{}
	// The orchestrator's stream route shares resolveRunLogsTarget with the
	// poll route, which reads req.query.offset -- not "from" (that name is
	// only ogre's OWN direct stream endpoint's param, wire contract section
	// 1). Sending "from" here silently asked for offset 0 every time.
	q.Set("offset", itoa64(offset))
	if attempt > 0 {
		q.Set("attempt", itoa(attempt))
	}
	path := "/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/logs/stream?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	setUserAgent(req)
	return req, nil
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

// RunArtifact is one entry of a run's landed artifacts: its log object, and
// every declared output. Key is a relative name, never the underlying S3 key
// or bucket layout -- mjolnir owns that (run-artifacts.ts), the orchestrator
// never hands it to a caller.
type RunArtifact struct {
	Key          string `json:"key"`
	SizeBytes    int64  `json:"sizeBytes"`
	LastModified string `json:"lastModified"`
}

// RunArtifactsList is GET /jobs/:id/runs/:runId/artifacts's response.
//
// Source is FOUR-state, deliberately mirroring GetRunLogs's own Source field:
// "no_attempt_yet" (the run has never had a box), "no_box" (this attempt's
// box was never assigned), "ok" (mjolnir answered -- which may still carry
// zero Artifacts, a legitimate answer for a run still executing), and
// "unreachable" (we could not ask). Collapsing "unreachable" into an empty
// "ok" list would tell a caller "no outputs" when the true answer is "we
// don't know yet" -- never do that locally either.
type RunArtifactsList struct {
	Source         string        `json:"source"`
	AttemptOrdinal *int          `json:"attemptOrdinal"`
	Artifacts      []RunArtifact `json:"artifacts"`
	Truncated      bool          `json:"truncated"`
}

// ListRunArtifacts lists one run's landed artifacts (the latest attempt,
// server-selected -- this client never sends its own ?attempt=, there is no
// CLI surface that picks one). Always inspect Source before trusting
// Artifacts; see RunArtifactsList's doc.
func (c *Client) ListRunArtifacts(jobID, runID string) (*RunArtifactsList, error) {
	var out RunArtifactsList
	path := "/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/artifacts"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RunArtifactDownload is GET .../artifacts/download?key=...'s response: a
// fresh, short-lived download URL for one artifact key named by a prior
// ListRunArtifacts response. Never stored anywhere -- it is a bearer
// credential, minted per request.
type RunArtifactDownload struct {
	URL            string `json:"url"`
	ExpiresAt      string `json:"expiresAt"`
	AttemptOrdinal int    `json:"attemptOrdinal"`
}

// DownloadRunArtifactURL mints a download URL for one artifact key. A key
// that no longer resolves (the run moved on, the box is gone) surfaces as an
// *APIError -- 404 for "no attempt"/"no box", 503 for "could not reach the
// box" (RunArtifactUnreachableError, orchestrator jobs.controller.ts) --
// which the caller must not treat as an empty artifact.
func (c *Client) DownloadRunArtifactURL(jobID, runID, key string) (*RunArtifactDownload, error) {
	var out RunArtifactDownload
	q := url.Values{}
	q.Set("key", key)
	path := "/jobs/" + url.PathEscape(jobID) + "/runs/" + url.PathEscape(runID) + "/artifacts/download?" + q.Encode()
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func itoa(i int) string     { return strconv.Itoa(i) }
func itoa64(i int64) string { return strconv.FormatInt(i, 10) }
