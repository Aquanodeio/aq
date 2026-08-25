package api

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// Funnel control endpoints used by `aq up`: SSH-key registration, the one-command
// `up` deploy, and deployment status polling. All require an authenticated Client
// (NewAuthed) — the orchestrator gates these on x-api-key (+ x-team-id for the
// team-scoped deployment routes).

// SSHKey mirrors a row from GET /settings/ssh-keys.
type SSHKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// ListSSHKeys returns the caller's registered SSH keys.
func (c *Client) ListSSHKeys() ([]SSHKey, error) {
	var out []SSHKey
	if err := c.getJSON("/settings/ssh-keys", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSSHKey registers a public key and returns the created row (with its id).
func (c *Client) CreateSSHKey(name, publicKey string) (*SSHKey, error) {
	var out SSHKey
	body := map[string]string{"name": name, "public_key": publicKey}
	if err := c.postJSON("/settings/ssh-keys", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpRequest is the body of POST /deployments/up.
type UpRequest struct {
	Template string  `json:"template"`
	SSHKeyID string  `json:"sshKeyId"`
	GPUModel string  `json:"gpuModel,omitempty"`
	MaxPrice float64 `json:"maxPrice,omitempty"`
	Provider string  `json:"provider,omitempty"`
	// Name sets the deployment's display name. When empty the orchestrator
	// falls back to a generated "<Adjective> <GPU> from <Region>" name. Passing
	// a stable `ticket-<N>-<label>` here lets the session-scoped reaper attribute
	// and clean up a throwaway box that would otherwise bill as an orphan (#310).
	Name string `json:"name,omitempty"`
	// IdlePolicy opts a fresh deployment into idle-auto-stop at creation time.
	// Nil (the Go zero value for a pointer) omits the key entirely, meaning "no
	// opinion, use the orchestrator's defaults," never "explicitly off." Those
	// defaults are OFF everywhere (DEFAULT_IDLE_POLICY.autoStopEnabled is false
	// and the console's deploy-sheet toggle starts unchecked), so a CLI deploy
	// that says nothing is not enrolled — same as every other surface.
	IdlePolicy *IdlePolicyUpdate `json:"idlePolicy,omitempty"`
}

// UpResult is the data returned by POST /deployments/up.
type UpResult struct {
	DeploymentID int    `json:"deploymentId"`
	ProjectID    string `json:"projectId"`
	Status       string `json:"status"`
	Message      string `json:"message"`
}

// Up rents the cheapest matching GPU and brings up the requested template env.
func (c *Client) Up(req UpRequest) (*UpResult, error) {
	var out UpResult
	if err := c.postJSON("/deployments/up", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeployRequest is the body of POST /deployments/deploy-snapshot — the
// OSS→compute bridge. It rents the cheapest matching GPU and restores a snapshot
// onto it, optionally relaunching an app template on the restored data (#180).
type DeployRequest struct {
	// SnapshotSource is a numeric deployment id or a synthetic `ext-<backupId>`
	// for a standalone-CLI snapshot (#177).
	SnapshotSource string  `json:"snapshotSource"`
	SSHKeyID       string  `json:"sshKeyId"`
	Template       string  `json:"template,omitempty"`
	GPUModel       string  `json:"gpuModel,omitempty"`
	MaxPrice       float64 `json:"maxPrice,omitempty"`
	Provider       string  `json:"provider,omitempty"`
	// Name sets the deployment's display name; empty → orchestrator-generated.
	// A stable `ticket-<N>-<label>` makes the throwaway box reapable (#310).
	Name string `json:"name,omitempty"`
}

// Deploy rents the cheapest matching GPU and restores the given snapshot onto it.
func (c *Client) Deploy(req DeployRequest) (*UpResult, error) {
	var out UpResult
	if err := c.postJSON("/deployments/deploy-snapshot", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ServiceCredentials is the running template service's reachable URL + auth, as
// surfaced on the deployment row once ogre has started the service.
type ServiceCredentials struct {
	Template string `json:"template"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	Status   string `json:"status"`
}

// SSHPort is the internal logical port a box's sshd is published on. Deployment
// rows key their service_urls entries by this port on every provider.
const SSHPort = 22

// ServiceURL is one entry of a deployment's service_urls array: a reachable URL
// keyed by the internal logical port it maps.
type ServiceURL struct {
	URL  string `json:"url"`
	Port portID `json:"port"`
}

// portID decodes a service_urls port that arrives as either a JSON number or a
// JSON string. An undecodable port yields 0 rather than an error: the value is
// only ever compared against SSHPort to pick an entry, and failing the whole
// Deployment decode over one odd port would break `aq up` status polling.
type portID int

func (p *portID) UnmarshalJSON(b []byte) error {
	n, err := strconv.Atoi(strings.Trim(string(b), `"`))
	if err != nil {
		*p = 0
		return nil
	}
	*p = portID(n)
	return nil
}

// Deployment is the subset of the deployment row `aq up` polling cares about.
//
// Every tag MUST stay in snake_case form. GET /deployments/:id/status runs rows
// through transformDeploymentData and so carries snake_case *plus* camelCase
// duplicates, but GET /deployments (the list) does not transform — its rows are
// snake_case only. A camelCase tag would decode the status endpoint and
// silently yield zero values for every list row.
type Deployment struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// ProjectID is only populated by GetDeployment (GET /deployments/:id, a
	// raw untransformed row) — the pause route is project-scoped rather than
	// deployment-scoped, so it's the one field that route needs beyond the
	// deployment id itself.
	ProjectID          string              `json:"project_id"`
	AppURL             string              `json:"app_url"`
	ServiceCredentials *ServiceCredentials `json:"service_credentials"`
	// ServiceURLs stays raw so a malformed or unexpectedly-shaped value (the
	// column is a free-form Json defaulting to "[]") can never fail the whole
	// row's decode. SSHServiceURL parses it on demand.
	ServiceURLs json.RawMessage `json:"service_urls"`
	// RestoreStatus/RestoreError carry the server-side snapshot restore outcome
	// for `aq deploy` (#235): SUCCESS / PARTIAL / FAILED, plus a detail string.
	// Empty for a plain `aq up` (no restore) or a backend that predates the field.
	RestoreStatus string `json:"restore_status"`
	RestoreError  string `json:"restore_error"`
	// RestoreCompatibility/RestoreWarnings carry the server-side CUDA/vendor
	// skew verdict computed on every restore (host_compat.go -> the deployment
	// row via vms/index.ts), previously computed and persisted but never
	// decoded here — a CLI user restoring a CUDA-12 environment onto a
	// CUDA-11 box saw nothing unless the restore hard-failed. Both fields stay
	// raw: their exact shape is not confirmed on the wire, and a decode
	// surprise here must never fail the whole Deployment/list decode — the
	// same defensive reasoning as ServiceURLs above. CompatibilityLabel/
	// RestoreWarningMessages parse them leniently on demand.
	RestoreCompatibility json.RawMessage `json:"restore_compatibility"`
	RestoreWarnings      json.RawMessage `json:"restore_warnings"`
}

// CompatibilityLabel parses RestoreCompatibility leniently: it may arrive as a
// bare string (e.g. "compatible"/"minor"/"major"/"vendor_mismatch") or be
// absent on a backend that predates the field. Any other shape yields "" —
// never an error, since this only ever feeds an informational print.
func (d Deployment) CompatibilityLabel() string {
	if len(d.RestoreCompatibility) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(d.RestoreCompatibility, &s); err == nil {
		return s
	}
	return ""
}

// RestoreWarningMessages parses RestoreWarnings leniently: the field may be a
// JSON array of strings, a single string, or absent. Never asserted to one
// shape — see the field's own doc comment for why.
func (d Deployment) RestoreWarningMessages() []string {
	if len(d.RestoreWarnings) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(d.RestoreWarnings, &list); err == nil {
		return list
	}
	var single string
	if err := json.Unmarshal(d.RestoreWarnings, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}

// SSHServiceURL returns the deployment's port-22 service URL.
//
// This is authoritative for SSH in a way app_url is not: simplepod silently
// publishes the ogre agent's HTTP port as app_url when the box maps no SSH port,
// so dialing app_url there reaches an HTTP server rather than sshd. The
// service_urls entry is keyed by the *internal* port, so port 22 is unambiguous
// and simply absent on a box with no SSH.
func (d Deployment) SSHServiceURL() (string, bool) {
	var urls []ServiceURL
	if err := json.Unmarshal(d.ServiceURLs, &urls); err != nil {
		return "", false
	}
	for _, u := range urls {
		if u.Port == SSHPort && u.URL != "" {
			return u.URL, true
		}
	}
	return "", false
}

// DeploymentStatusResult mirrors GET /deployments/:id/status.
type DeploymentStatusResult struct {
	DeploymentID int        `json:"deploymentId"`
	Status       string     `json:"status"`
	Deployment   Deployment `json:"deployment"`
}

// DeploymentStatus fetches the current status of a deployment.
func (c *Client) DeploymentStatus(deploymentID int) (*DeploymentStatusResult, error) {
	var out DeploymentStatusResult
	path := "/deployments/" + strconv.Itoa(deploymentID) + "/status"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetProjectDeployment returns the current (active or paused) deployment under a
// project, via GET /deployments/project/:projectId. `aq down`/`aq status` use it
// to accept a project id (the UUID in the console URL) in place of the numeric
// deployment id (#209). A project with no live deployment yields a 404 APIError.
func (c *Client) GetProjectDeployment(projectID string) (*Deployment, error) {
	var out Deployment
	path := "/deployments/project/" + url.PathEscape(projectID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListDeployments returns every deployment on the caller's team, newest first.
//
// Two properties of GET /deployments to keep in mind: it takes no filters (the
// documented `type`/`provider`/`sortByTime` query params are parsed by a schema
// the controller never reads), and it returns the team's FULL history including
// CLOSED and FAILED rows. Callers must filter client-side, and should only call
// it where a list is genuinely needed — on a long-lived team it is a large
// response.
func (c *Client) ListDeployments() ([]Deployment, error) {
	var out []Deployment
	if err := c.getJSON("/deployments", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetDeployment fetches the raw deployment row by id (GET /deployments/:id),
// including ProjectID — the fields the transformed /status and list endpoints
// don't carry.
func (c *Client) GetDeployment(deploymentID int) (*Deployment, error) {
	var out Deployment
	path := "/deployments/" + strconv.Itoa(deploymentID)
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ParkDeployment saves the deployment's current state and releases its
// rented box (`aq park`), via the existing project-scoped pause route — the
// route and its path are still named "pause" server-side, but the CLI verb
// is "park" (see park.go's doc comment for why). Pause/park always targets
// the ACTIVE deployment under a project, so it's keyed by projectID in the
// path plus deploymentID in the body, not by deployment id alone.
func (c *Client) ParkDeployment(projectID string, deploymentID int) error {
	path := "/deployments/project/" + url.PathEscape(projectID) + "/pause"
	body := map[string]int{"deploymentId": deploymentID}
	return c.postJSON(path, body, nil)
}

// CloseResult mirrors POST /deployments/close.
type CloseResult struct {
	Status string `json:"status"`
}

// CloseDeployment requests termination of a deployment (`aq down`). The
// orchestrator validates team ownership and tears the box down asynchronously.
func (c *Client) CloseDeployment(deploymentID int) (*CloseResult, error) {
	var out CloseResult
	body := map[string]int{"deploymentId": deploymentID}
	if err := c.postJSON("/deployments/close", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SnapshotHistoryBackup is the `backups` object nested on a history item,
// carrying the source deployment this snapshot belongs to. It is nil for an
// external/CLI snapshot (no Aquanode deployment).
type SnapshotHistoryBackup struct {
	DeploymentID int    `json:"deployment_id"`
	Path         string `json:"path"`
}

// SnapshotHistoryItem mirrors one row of GET /snapshots/history. The top-level
// BackupID is the internal backup ROW id — NOT a deployment id; a snapshot is
// associated with its owning deployment via Backups.DeploymentID instead.
type SnapshotHistoryItem struct {
	ID        int                    `json:"id"`
	BackupID  int                    `json:"backup_id"`
	Path      string                 `json:"path"`
	Status    string                 `json:"status"`
	Size      int64                  `json:"size"`
	Type      string                 `json:"type"`
	CreatedAt string                 `json:"created_at"`
	Backups   *SnapshotHistoryBackup `json:"backups"`
}

// SnapshotHistory lists every snapshot the account owns, including external/CLI
// ones with no source deployment.
func (c *Client) SnapshotHistory() ([]SnapshotHistoryItem, error) {
	var out []SnapshotHistoryItem
	if err := c.getJSON("/snapshots/history", &out); err != nil {
		return nil, err
	}
	return out, nil
}
