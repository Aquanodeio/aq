package api

// Import endpoints backing `aq import`, bringing a box running somewhere else
// (RunPod, Vast, a bare-metal box, ...) into Aquanode as a real Volume. The
// observation/survey types below mirror ogre's own wire contract (referenced
// elsewhere as CONTRACT.md section C; do not rename one of those without
// checking ogre and the orchestrator first, since both implement against it
// independently). The Start/Credentials/Complete request+response shapes
// were retargeted from /setups/import/* to /volumes/import/* per the
// pod/environment/volume plan's D12 (POST /snapshots/external ->
// POST /volumes/import): a captured box is now adopted as a Volume, not a
// whole Setup with a synthesized recipe.
//
// The observation aq decodes from `ogre capture`'s own stdout is re-encoded
// through these Go structs before it's forwarded to the orchestrator (it is
// NOT passed through as raw, untouched bytes), so a field's Go TYPE decides
// what actually reaches the wire: a non-pointer field can never distinguish
// "ogre didn't report this" from "ogre reported the zero value", and would
// silently send a fabricated false answer either way.

import "net/url"

// ImportObservationSchema is the only ImportObservation.Schema value this aq
// build understands. The contract requires every consumer to reject an
// unknown schema loudly rather than guess at a shape it was never told about.
const ImportObservationSchema = 1

// ImportHost is the observed host's identity, as `ogre capture` reports it.
// Every field is independently nullable on the wire (importObservationSchema,
// orchestrator/src/schemas/volumes.schemas.ts:28-51, confirmed against
// source, aquanode-backend#803): ogre can report some facts about a host
// without all of them (hostname known, kernel version not, say), so each
// field is a pointer with omitempty, never a bare string/int that would
// decode a field ogre never sent as "" or 0 and then re-send that as if it
// were a real answer.
type ImportHost struct {
	Hostname  *string `json:"hostname,omitempty"`
	OS        *string `json:"os,omitempty"`
	Kernel    *string `json:"kernel,omitempty"`
	CPUCores  *int    `json:"cpu_cores,omitempty"`
	MemoryGB  *int    `json:"memory_gb,omitempty"`
	StorageGB *int    `json:"storage_gb,omitempty"`
}

// ImportGPU is the observed GPU, if any. Skew is "unknown" (never "none")
// when no GPU is visible, absence is not a match, per the contract. Every
// field is independently nullable on the wire, same reasoning as ImportHost.
type ImportGPU struct {
	Vendor      *string `json:"vendor,omitempty"`
	Name        *string `json:"name,omitempty"`
	Count       *int    `json:"count,omitempty"`
	DriverCUDA  *string `json:"driver_cuda,omitempty"`
	ToolkitCUDA *string `json:"toolkit_cuda,omitempty"`
	ROCmVersion *string `json:"rocm_version,omitempty"`
	ComputeCap  *string `json:"compute_cap,omitempty"`
	Skew        *string `json:"skew,omitempty"`
}

// ImportApp is the workload ogre's DetectApp found on the box. It is nil
// (Observation.App) when nothing was detected — never fabricated.
type ImportApp struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Dir        string `json:"dir"`
	Port       int    `json:"port"`
	HealthPath string `json:"health_path"`
}

// ImportCapture is what ogre captured (or was told to capture) and from where.
type ImportCapture struct {
	MountPath string   `json:"mount_path"`
	Paths     []string `json:"paths"`
	Excludes  []string `json:"excludes"`
}

// ImportCaptureEntry is one path counted into the "capturing" survey block.
type ImportCaptureEntry struct {
	Path string `json:"path"`
	// Bytes is a FLOOR, not an exact size, when BytesTruncated is true — the
	// walk hit a budget partway through this path. Render as ">= N" then.
	Bytes          int64  `json:"bytes"`
	BytesTruncated bool   `json:"bytes_truncated"`
	Source         string `json:"source"` // "detected" | "explicit"
}

// ImportSkippedEntry is one path counted into the "not_capturing" survey
// block — the actionable one: a user who sees a large skipped directory here
// either --includes it or accepts the loss knowingly.
type ImportSkippedEntry struct {
	Path           string `json:"path"`
	Bytes          int64  `json:"bytes"`
	BytesTruncated bool   `json:"bytes_truncated"`
	Reason         string `json:"reason"`
}

// ImportUnreadableEntry is one path ogre could not even stat/list —
// permission denied, most often. This is a THIRD state, distinct from
// "not_capturing": a path we couldn't read must never be silently dropped or
// reported as "skipped by choice."
type ImportUnreadableEntry struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ImportSurvey is the full pre-capture report: what will be captured, what
// won't, what couldn't even be read, and whether the walk itself ran out of
// budget before finishing (in which case every size above is a floor).
//
// Capturing and Unreadable are mutually exclusive at the top level (contract
// H1, ogre 94d34a5): a capture path whose own root cannot be opened is
// dropped from Capturing and reported in Unreadable only — never both, which
// would otherwise claim capture of a tree that couldn't be read. A path whose
// root IS readable stays in Capturing even when some descendants fail; those
// descendants are listed individually in Unreadable (partial capture is real
// capture, not discarded).
type ImportSurvey struct {
	Capturing    []ImportCaptureEntry    `json:"capturing"`
	NotCapturing []ImportSkippedEntry    `json:"not_capturing"`
	Unreadable   []ImportUnreadableEntry `json:"unreadable"`
	// MinReportBytes is the size floor NotCapturing was built with (contract
	// H2, default 1 GiB). Every renderer of this survey MUST state it:
	// without it the "not capturing" block reads as EXHAUSTIVE, when a
	// directory under the floor appears in neither list and nothing says a
	// floor was ever applied — the same class of failure as a silent skip.
	MinReportBytes int64 `json:"min_report_bytes"`
	WalkTruncated  bool  `json:"walk_truncated"`
	DeadlineHit    bool  `json:"deadline_hit"`
}

// Incomplete reports whether the survey hit a budget before it could finish
// walking the box — if so, every size in the survey is a floor, and paths
// past the budget may be missing from either block entirely.
func (s ImportSurvey) Incomplete() bool {
	return s.WalkTruncated || s.DeadlineHit
}

// ImportPythonEnv is one recorded Python environment's package manifest. This
// is recorded for the user's reference only — the manifest is never replayed.
type ImportPythonEnv struct {
	Env       string   `json:"env"`
	Kind      string   `json:"kind"` // "venv" | "conda"
	Packages  []string `json:"packages"`
	Truncated bool     `json:"truncated"`
}

// ImportSystemPackages is the observed OS package manager's install list.
type ImportSystemPackages struct {
	Manager   string   `json:"manager"` // "dpkg" | "rpm" | "none"
	Packages  []string `json:"packages"`
	Truncated bool     `json:"truncated"`
}

// ImportManifest is the "recorded but not restorable" package manifest — a
// reference for the user to rebuild by hand, never a restore instruction.
type ImportManifest struct {
	Collected      bool                 `json:"collected"`
	PythonEnvs     []ImportPythonEnv    `json:"python_envs"`
	SystemPackages ImportSystemPackages `json:"system_packages"`
}

// ImportObservation is what `ogre capture` observed on the foreign box
// (CONTRACT.md section A). aq decodes it to render the survey, then
// re-encodes the SAME decoded value when forwarding it to the orchestrator
// on completion, it is not raw passthrough bytes, so every field here has to
// carry real nullability (see ImportHost/ImportGPU) or a fact ogre never
// reported gets fabricated as a zero value on the way back out.
type ImportObservation struct {
	Schema   int            `json:"schema"`
	Host     ImportHost     `json:"host"`
	GPU      ImportGPU      `json:"gpu"`
	App      *ImportApp     `json:"app"`
	Capture  ImportCapture  `json:"capture"`
	Survey   ImportSurvey   `json:"survey"`
	Manifest ImportManifest `json:"manifest"`
}

// ImportCredentials are scoped, time-limited write credentials for the new
// volume's storage prefix (minted by /volumes/import/start and re-mintable
// via /volumes/import/credentials for an upload that outlives one minting).
type ImportCredentials struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Region          string `json:"region"`
}

// ImportStartRequest is the body of POST /volumes/import/start. Both fields
// are optional: the orchestrator names the volume and picks a mount path
// when they're empty.
type ImportStartRequest struct {
	Name      string `json:"name,omitempty"`
	MountPath string `json:"mount_path,omitempty"`
}

// ImportStartResult is the data returned by POST /volumes/import/start: a
// real Volume already exists at this point, with a real (billed, visible,
// deletable) storage prefix, before a single byte has been captured. This is
// the pod/environment/volume plan's D12: the route used to be
// POST /snapshots/external and mint a whole Setup; a box captured from
// outside Aquanode is now just its /workspace data, adopted as a Volume
// (the setup-adopt.service.ts pattern) -- the Environment/recipe half of the
// old flow is gone, since a bare import carries no installed-package
// manifest to synthesize one from.
//
// ResticBackupID is the SERVER's own convention for the trailing path segment
// of the restic repo (`setup.service.ts`'s resticRepositoryUrl: currently the
// literal "repo" for every portable setup, since StoragePrefix already makes
// the repo unique — see CONTRACT.md section G). aq passes it straight through
// to `ogre capture` uninterpreted; it must NEVER be guessed or defaulted
// client-side; a value like the volume's own uuid writes to a path nothing
// ever reads, and the uploaded bytes then sit there billing forever with no
// error anywhere.
type ImportStartResult struct {
	VolumeID       string            `json:"volume_id"`
	StoragePrefix  string            `json:"storage_prefix"`
	ResticPassword string            `json:"restic_password"`
	ResticBackupID string            `json:"restic_backup_id"`
	ImportToken    string            `json:"import_token"`
	ExpiresAt      string            `json:"expires_at"`
	Credentials    ImportCredentials `json:"credentials"`
}

// StartImport creates the Volume that will receive the capture and mints
// scoped write credentials + a single-use completion token for it.
func (c *Client) StartImport(req ImportStartRequest) (*ImportStartResult, error) {
	var out ImportStartResult
	if err := c.postJSON("/volumes/import/start", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportCredentialsRefreshRequest is the body of POST
// /volumes/import/credentials: re-mints EVERYTHING `aq import --resume`
// needs for a still-pending import, keyed by volume id alone.
type ImportCredentialsRefreshRequest struct {
	VolumeID string `json:"volume_id"`
}

// ImportCredentialsRefreshResult is the data returned by POST
// /volumes/import/credentials. This is the WHOLE point of the route: it
// returns everything --resume needs: StoragePrefix/ResticBackupID/
// ResticPassword alongside a freshly-minted ImportToken and scoped write
// Credentials, so aq never has to persist a single one of these locally.
// ImportToken here SUPERSEDES any token from a prior /start or /credentials
// call for this volume; using a remembered one for /complete will be
// refused.
type ImportCredentialsRefreshResult struct {
	VolumeID       string            `json:"volume_id"`
	StoragePrefix  string            `json:"storage_prefix"`
	ResticBackupID string            `json:"restic_backup_id"`
	ResticPassword string            `json:"restic_password"`
	ImportToken    string            `json:"import_token"`
	ExpiresAt      string            `json:"expires_at"`
	Credentials    ImportCredentials `json:"credentials"`
}

// RefreshImportCredentials re-mints everything needed to resume volumeID's
// still-pending import: scoped write credentials, the storage location, and a
// fresh single-use completion token. This is `aq import --resume`'s ONLY
// source of that state — aq keeps no local copy of any of it.
func (c *Client) RefreshImportCredentials(volumeID string) (*ImportCredentialsRefreshResult, error) {
	var out ImportCredentialsRefreshResult
	if err := c.postJSON("/volumes/import/credentials", ImportCredentialsRefreshRequest{VolumeID: volumeID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportCompleteRequest is the body of POST /volumes/import/complete
// (completeImportSchema, orchestrator/src/schemas/volumes.schemas.ts:103-108,
// confirmed against source, aquanode-backend#803): exactly volume_id,
// import_token, ogre_snapshot_id, and observation. `ogre capture` also
// reports a path and a size on its own stdout (ogreCaptureOutput), but the
// orchestrator's schema never defined those keys, so they are not sent here
// at all, never a field that gets silently stripped server-side (rule 4,
// delete never alias).
type ImportCompleteRequest struct {
	VolumeID       string            `json:"volume_id"`
	ImportToken    string            `json:"import_token"`
	OgreSnapshotID string            `json:"ogre_snapshot_id"`
	Observation    ImportObservation `json:"observation"`
}

// ImportCompleteResult is the data returned by POST /volumes/import/complete.
// There is no recipe/version here any more (that was the old Setup-shaped
// flow's synthesized launch config). A Volume carries /workspace data only,
// nothing installable, so nothing is synthesized on completion.
type ImportCompleteResult struct {
	VolumeID string   `json:"volume_id"`
	Warnings []string `json:"warnings"`
}

// CompleteImport registers the capture as the volume's first history point,
// consuming the single-use import token. The token is deleted server-side on
// read, so a retried call after a transport error (rather than a genuine
// second import) will be refused. That's a real gap in this v1 client,
// noted rather than papered over.
func (c *Client) CompleteImport(req ImportCompleteRequest) (*ImportCompleteResult, error) {
	var out ImportCompleteResult
	if err := c.postJSON("/volumes/import/complete", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// OgreDownloadURLResult is the data returned by GET
// /artifacts/ogre/download-url: a presigned URL for the pinned ogre release,
// good only until ExpiresAt, plus the sha256 aq MUST verify before executing
// anything it downloads.
type OgreDownloadURLResult struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
	SHA256    string `json:"sha256"`
	Version   string `json:"version"`
	// Asset is the file the URL serves, e.g. ogre_Darwin_arm64.tar.gz. It is
	// the wire's own statement that this is a TAR.GZ ARCHIVE for one platform
	// rather than a bare binary — the thing nothing on this path ever said,
	// which is why aq installed the tarball itself as the executable and every
	// `aq import` died with `exec format error` (#970).
	Asset string `json:"asset"`
}

// OgreDownloadURL fetches a presigned download URL for the ogre binary this
// account's orchestrator expects, for a laptop that has no `ogre` on PATH.
//
// goos/goarch are the CALLER's platform (runtime.GOOS / runtime.GOARCH). The
// server selects the matching build and answers 404 when it publishes none —
// which the caller must surface as its own refusal, because there is no
// runnable second choice.
func (c *Client) OgreDownloadURL(goos, goarch string) (*OgreDownloadURLResult, error) {
	var out OgreDownloadURLResult
	path := "/artifacts/ogre/download-url?os=" + url.QueryEscape(goos) + "&arch=" + url.QueryEscape(goarch)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
