package api

import (
	"net/url"
	"strconv"
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

// Deployment is the subset of the deployment row `aq up` polling cares about.
type Deployment struct {
	ID                 int                 `json:"id"`
	Status             string              `json:"status"`
	AppURL             string              `json:"app_url"`
	ServiceCredentials *ServiceCredentials `json:"service_credentials"`
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
