package api

// Import endpoints backing `aq import` — bringing a box running somewhere else
// (RunPod, Vast, a bare-metal box, ...) into Aquanode as a real Setup. Every
// type here mirrors the frozen wire contract in CONTRACT.md section C; do not
// rename a field without checking that contract first, since ogre and the
// orchestrator implement against it independently.

// ImportObservationSchema is the only ImportObservation.Schema value this aq
// build understands. The contract requires every consumer to reject an
// unknown schema loudly rather than guess at a shape it was never told about.
const ImportObservationSchema = 1

// ImportHost is the observed host's identity, as `ogre capture` reports it.
type ImportHost struct {
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Kernel    string `json:"kernel"`
	CPUCores  int    `json:"cpu_cores"`
	MemoryGB  int    `json:"memory_gb"`
	StorageGB int    `json:"storage_gb"`
}

// ImportGPU is the observed GPU, if any. Skew is "unknown" (never "none")
// when no GPU is visible — absence is not a match, per the contract.
type ImportGPU struct {
	Vendor      string `json:"vendor"`
	Name        string `json:"name"`
	Count       int    `json:"count"`
	DriverCUDA  string `json:"driver_cuda"`
	ToolkitCUDA string `json:"toolkit_cuda"`
	ROCmVersion string `json:"rocm_version"`
	ComputeCap  string `json:"compute_cap"`
	Skew        string `json:"skew"`
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
type ImportSurvey struct {
	Capturing     []ImportCaptureEntry    `json:"capturing"`
	NotCapturing  []ImportSkippedEntry    `json:"not_capturing"`
	Unreadable    []ImportUnreadableEntry `json:"unreadable"`
	WalkTruncated bool                    `json:"walk_truncated"`
	DeadlineHit   bool                    `json:"deadline_hit"`
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
// (CONTRACT.md section A). aq decodes it only to render the survey and to
// pull the observed GPU model for `--launch` — it is otherwise passed
// VERBATIM to the orchestrator on completion, never re-encoded through a
// narrower struct that could silently drop a field the orchestrator relies on.
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
// setup's storage prefix — minted by /setups/import/start and re-mintable via
// /setups/import/credentials for an upload that outlives one minting.
type ImportCredentials struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Region          string `json:"region"`
}

// ImportStartRequest is the body of POST /setups/import/start. Both fields are
// optional — the orchestrator names the setup and picks a mount path when
// they're empty.
type ImportStartRequest struct {
	Name      string `json:"name,omitempty"`
	MountPath string `json:"mount_path,omitempty"`
}

// ImportStartResult is the data returned by POST /setups/import/start: a real
// Setup already exists at this point, with a real (billed, visible, deletable)
// storage prefix, before a single byte has been captured.
//
// ResticBackupID is the SERVER's own convention for the trailing path segment
// of the restic repo (`setup.service.ts`'s resticRepositoryUrl: currently the
// literal "repo" for every portable setup, since StoragePrefix already makes
// the repo unique — see CONTRACT.md section G). aq passes it straight through
// to `ogre capture` uninterpreted; it must NEVER be guessed or defaulted
// client-side; a value like the setup's own uuid writes to a path nothing
// ever reads, and the uploaded bytes then sit there billing forever with no
// error anywhere.
type ImportStartResult struct {
	SetupID        string            `json:"setup_id"`
	StoragePrefix  string            `json:"storage_prefix"`
	ResticPassword string            `json:"restic_password"`
	ResticBackupID string            `json:"restic_backup_id"`
	ImportToken    string            `json:"import_token"`
	ExpiresAt      string            `json:"expires_at"`
	Credentials    ImportCredentials `json:"credentials"`
}

// StartImport creates the Setup that will receive the capture and mints
// scoped write credentials + a single-use completion token for it.
func (c *Client) StartImport(req ImportStartRequest) (*ImportStartResult, error) {
	var out ImportStartResult
	if err := c.postJSON("/setups/import/start", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportCredentialsRefreshRequest is the body of POST
// /setups/import/credentials — re-mints scoped write credentials for a still-
// pending import. StoragePrefix/ResticPassword/ResticBackupID are unaffected
// by this call; only the S3 write credentials themselves are time-limited.
type ImportCredentialsRefreshRequest struct {
	SetupID string `json:"setup_id"`
}

// ImportCredentialsRefreshResult is the data returned by POST
// /setups/import/credentials.
type ImportCredentialsRefreshResult struct {
	Credentials ImportCredentials `json:"credentials"`
	ExpiresAt   string            `json:"expires_at"`
}

// RefreshImportCredentials re-mints scoped write credentials for setupID's
// still-pending import — the mechanism `aq import --resume` uses when the
// credentials minted at StartImport have expired before the capture finished.
func (c *Client) RefreshImportCredentials(setupID string) (*ImportCredentialsRefreshResult, error) {
	var out ImportCredentialsRefreshResult
	if err := c.postJSON("/setups/import/credentials", ImportCredentialsRefreshRequest{SetupID: setupID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportCompleteRequest is the body of POST /setups/import/complete. The
// observation is forwarded exactly as `ogre capture` emitted it.
type ImportCompleteRequest struct {
	SetupID        string            `json:"setup_id"`
	ImportToken    string            `json:"import_token"`
	OgreSnapshotID string            `json:"ogre_snapshot_id"`
	Path           string            `json:"path"`
	Size           int64             `json:"size"`
	Observation    ImportObservation `json:"observation"`
}

// ImportCompleteResult is the data returned by POST /setups/import/complete.
// Recipe stays raw: it's the orchestrator's internal SetupRecipe shape, which
// aq has no reason to decode — it only ever gets restored server-side.
type ImportCompleteResult struct {
	SetupID   string   `json:"setup_id"`
	VersionID int      `json:"version_id"`
	Recipe    any      `json:"recipe"`
	Warnings  []string `json:"warnings"`
}

// CompleteImport registers the version, consuming the single-use import
// token. The token is deleted server-side on read, so a retried call after a
// transport error (rather than a genuine second import) will be refused —
// that's a real gap in this v1 client, noted rather than papered over.
func (c *Client) CompleteImport(req ImportCompleteRequest) (*ImportCompleteResult, error) {
	var out ImportCompleteResult
	if err := c.postJSON("/setups/import/complete", req, &out); err != nil {
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
}

// OgreDownloadURL fetches a presigned download URL for the ogre binary this
// account's orchestrator expects, for a laptop that has no `ogre` on PATH.
func (c *Client) OgreDownloadURL() (*OgreDownloadURLResult, error) {
	var out OgreDownloadURLResult
	if err := c.getJSON("/artifacts/ogre/download-url", &out); err != nil {
		return nil, err
	}
	return &out, nil
}
