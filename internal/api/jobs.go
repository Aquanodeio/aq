package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Jobs and their Runs, backing `aq job` and `aq endpoint`. Per the
// jobs-are-jobs spec, a BATCH job
// (command entrypoint) is 1:1 with its one Run: `aq job run` creates both in
// one call, and running it again always makes a NEW job via `aq job rerun`.
// A SERVICE job (http/comfyui entrypoint, `aq endpoint`) is unaffected and
// keeps its own scaling knobs (MaxInstances, MinInstances) on the Job type
// below.
//
// Unlike setups.go's snake_case DTOs, these routes speak camelCase on the
// wire — match the field names exactly (versionId, gpuModels, ...), do
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

// CreateBatchJobRequest is the body of POST /jobs for a command (batch)
// entrypoint, per the jobs-are-jobs spec:
// creating a batch job creates its ONE Run in the same transaction, so there
// is no separate "now start it" call any more, and no per-job scaling
// knob to size — MaxInstances and MonthlySpendCapCents are GONE from this
// request entirely, not merely left unset, because the backend 400s a batch
// create that carries either key by name. Keeping them as accepted no-ops
// would be worse than removing them: a struct field is a promise the wire
// still means something for it, and it does not any more.
//
// PinnedDeploymentID pins the job to a box the customer already owns
// (attached via `aq host add` + `aq attach`) instead of hardware Aquanode
// rents. It carries `omitempty` deliberately: the zero value must never
// reach the wire as a present-but-empty key, only as an absent one, the
// server reads an absent key as "today's managed behaviour" and a present
// zero/negative one as a malformed pin. jobRun resolves this from a
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
type CreateBatchJobRequest struct {
	Name               string        `json:"name"`
	VersionID          int           `json:"versionId,omitempty"`
	Image              *ImageSource  `json:"image,omitempty"`
	Entrypoint         *Entrypoint   `json:"entrypoint,omitempty"`
	Hardware           *Hardware     `json:"hardware,omitempty"`
	Placement          *JobPlacement `json:"placement,omitempty"`
	PinnedDeploymentID int           `json:"pinnedDeploymentId,omitempty"`
	// Secrets names `type: "env"` team secrets (POST /secrets/teams/:teamId,
	// see internal/api/secrets.go) this job's Run needs injected at dispatch.
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
// run` only ever emits `command`. Argv comes verbatim from everything
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
// hardware.ts PlacementSchema. Named JobPlacement rather than the bare
// Placement, so a future concept sharing that name (control.go used to have
// one, for a deploy-snapshot resume the pod/environment/volume plan retired)
// never collides with it by accident. GPUOrder carries `omitempty`: an
// absent key already means "cheapest" (today's behaviour), so that value is
// never written explicitly, exactly as the console omits it.
type JobPlacement struct {
	GPUOrder string `json:"gpuOrder,omitempty"`
}

// BatchJob mirrors one element of `GET /jobs?shape=batch`, the object
// `GET /jobs/:id` returns for a batch job, and the 201 body of `POST /jobs`
// and `POST /jobs/:id/rerun` for a command entrypoint — the jobs-are-jobs
// spec's batch wire shape.
//
// This is a SEPARATE type from Job, never a widened version of it: Job is
// the service-shape row `aq endpoint` still reads (status, runUrl,
// runningInstances, the scaling/schedule/token fields), and every one of
// those is explicitly ABSENT from a batch row's body under the new
// contract — a shared struct would silently decode zero values for fields
// that were never sent, which reads exactly like a row that legitimately
// has none. Run is ALWAYS present, never null: a batch job is 1:1 with the
// one Run it was created with.
type BatchJob struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Shape     string `json:"shape"`
	CreatedAt string `json:"createdAt"`
	Run       Run    `json:"run"`
}

// ListBatchJobs returns the caller's batch (command-entrypoint) jobs only —
// GET /jobs?shape=batch. `aq job ls` must never show a service (endpoint)
// row: the two read as unrelated concepts to a caller, same reasoning as
// ListJobsByShape("service") for `aq endpoint list`.
func (c *Client) ListBatchJobs() ([]BatchJob, error) {
	var out []BatchJob
	if err := c.getJSON("/jobs?shape=batch", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateBatchJob creates a batch job AND its one Run in a single call —
// POST /jobs with a command entrypoint. Unlike the old create-then-run
// (create)/(job runs) split, this is the only way to make a batch job at
// all now: there is no follow-up call that starts it.
func (c *Client) CreateBatchJob(req CreateBatchJobRequest) (*BatchJob, error) {
	var out BatchJob
	if err := c.postJSON("/jobs", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RerunJob makes a NEW job copying the source job's spec, plus its own new
// Run — POST /jobs/:id/rerun. "Running it again" always means a new job
// under this contract (jobs-are-jobs spec, "The contract" section); the
// source job named here is never mutated.
func (c *Client) RerunJob(jobID string) (*BatchJob, error) {
	var out BatchJob
	path := "/jobs/" + url.PathEscape(jobID) + "/rerun"
	if err := c.postJSON(path, struct{}{}, &out); err != nil {
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
// the service-shaped sibling of CreateBatchJobRequest. A deliberately separate
// type rather than reusing CreateBatchJobRequest's `Entrypoint *Entrypoint`
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
	// CostCents is the billed cost of this run in US cents, computed
	// server-side from the ledger (round(sum(billing_buckets_v2.amount_usd)
	// * 100) over this run's attempts' deployments). nil means UNKNOWN --
	// the run has no attempt with a deployment yet, or every such
	// deployment is customer-owned (unmetered) -- never "free" or "$0.00".
	// Clients render this number and never compute cost themselves.
	CostCents *int64 `json:"costCents"`
	// Attempts is the run's own history (run.service.ts serializeRunForOwner):
	// a failover shows up here as more than one entry, each with its own
	// provider, rate and window. `aq job ls` derives its GPU/duration
	// columns from this. omitempty because the CALLER's view
	// (serializeRunForCaller, what a run-token holder sees) never carries
	// this key at all.
	Attempts []RunAttempt `json:"attempts,omitempty"`
}

// RunAttempt is one entry of Run.Attempts. Only the fields `aq job ls` and a
// future run-detail view need are named here; the owner view carries more
// (checkpoint bookkeeping, stall verdicts) that no CLI surface renders yet —
// an unlisted field simply decodes as absent, never breaking this struct.
type RunAttempt struct {
	ID       string `json:"id"`
	Ordinal  int    `json:"ordinal"`
	Provider string `json:"provider"`
	// GPUModel and PriceCentsPerHour are nullable on the wire (an attempt
	// that never got a box has neither yet), so both are pointers -- nil
	// means UNKNOWN, never "free" or "no GPU".
	GPUModel          *string `json:"gpuModel"`
	PriceCentsPerHour *int64  `json:"priceCentsPerHour"`
	PlacedAt          string  `json:"placedAt"`
	StartedAt         *string `json:"startedAt"`
	EndedAt           *string `json:"endedAt"`
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
