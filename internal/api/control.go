package api

import "strconv"

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
