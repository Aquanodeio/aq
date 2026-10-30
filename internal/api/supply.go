package api

import (
	"encoding/json"
	"net/url"
	"strconv"
)

// Wait-for-supply endpoints. Two server-side features share one idea: the
// request lives on the server, so it keeps working while the caller's laptop
// sleeps.
//
//   - /availability-watches: notify-when-available. Spends nothing; flips to
//     NOTIFIED when an offer matching it appears.
//   - /queued-deployments: deploy-when-available. Fires a real create when an
//     offer at or under the price ceiling appears.
//
// Both match the GPU model name EXACTLY and the GPU count EXACTLY (not "at
// least"), and both compare the price ceiling against the WHOLE offer's hourly
// price, not a per-GPU rate.

// AvailabilityWatch is one row of /availability-watches. Raw holds the row
// exactly as the server sent it, so `--json` can print the server's own keys
// rather than a re-marshal that drops any this struct does not know.
type AvailabilityWatch struct {
	ID                  int      `json:"id"`
	GPUModel            string   `json:"gpuModel"`
	GPUCount            int      `json:"gpuCount"`
	MaxPricePerHour     *float64 `json:"maxPricePerHour"`
	ProviderScope       *string  `json:"providerScope"`
	Tier                *string  `json:"tier"`
	Status              string   `json:"status"` // WATCHING | NOTIFIED | EXPIRED | CANCELLED
	ExpiresAt           string   `json:"expiresAt"`
	NotifiedAt          *string  `json:"notifiedAt"`
	MatchedProvider     *string  `json:"matchedProvider"`
	MatchedPricePerHour *float64 `json:"matchedPricePerHour"`
	CreatedAt           string   `json:"createdAt"`
	ResolvedAt          *string  `json:"resolvedAt"`
	IncludeUnreported   bool     `json:"includeUnreported"`

	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the row and keeps its raw bytes.
func (w *AvailabilityWatch) UnmarshalJSON(b []byte) error {
	type plain AvailabilityWatch
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*w = AvailabilityWatch(p)
	w.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// ArmWatchRequest is the body of POST /availability-watches. Every optional
// field is omitted when unset: the server's zod enums reject "" and a ceiling
// of 0 is not "no ceiling".
type ArmWatchRequest struct {
	GPUModel        string   `json:"gpuModel"`
	GPUCount        int      `json:"gpuCount"`
	MaxPricePerHour *float64 `json:"maxPricePerHour,omitempty"`
	ProviderScope   string   `json:"providerScope,omitempty"`
	Tier            string   `json:"tier,omitempty"`
	ExpiresInHours  *float64 `json:"expiresInHours,omitempty"`
	// IncludeUnreported is sent only when true; the key is absent otherwise.
	IncludeUnreported bool `json:"includeUnreported,omitempty"`
}

// ArmAvailabilityWatch arms a notify-when-available watch.
func (c *Client) ArmAvailabilityWatch(req ArmWatchRequest) (*AvailabilityWatch, error) {
	var out AvailabilityWatch
	if err := c.postJSON("/availability-watches", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAvailabilityWatch reads one watch in ANY status. A 404 means it is
// missing or belongs to another team.
func (c *Client) GetAvailabilityWatch(id int) (*AvailabilityWatch, error) {
	var out AvailabilityWatch
	if err := c.getJSON("/availability-watches/"+url.PathEscape(strconv.Itoa(id)), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAvailabilityWatches lists the team's WATCHING rows.
func (c *Client) ListAvailabilityWatches() ([]AvailabilityWatch, error) {
	var out []AvailabilityWatch
	if err := c.getJSON("/availability-watches", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CancelAvailabilityWatch cancels an active watch; a 404 means it is not active.
func (c *Client) CancelAvailabilityWatch(id int) error {
	return c.deleteJSON("/availability-watches/"+url.PathEscape(strconv.Itoa(id)), nil)
}

// QueuedDeployment is one row of /queued-deployments.
type QueuedDeployment struct {
	ID                  int     `json:"id"`
	GPUModel            string  `json:"gpuModel"`
	GPUCount            int     `json:"gpuCount"`
	PriceCeilingPerHour float64 `json:"priceCeilingPerHour"`
	ProviderScope       *string `json:"providerScope"`
	Status              string  `json:"status"`
	ExpiresAt           string  `json:"expiresAt"`
	DeploymentID        *int    `json:"deploymentId"`
	LastError           *string `json:"lastError"`
	CreatedAt           string  `json:"createdAt"`

	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the row and keeps its raw bytes.
func (q *QueuedDeployment) UnmarshalJSON(b []byte) error {
	type plain QueuedDeployment
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*q = QueuedDeployment(p)
	q.Raw = append(json.RawMessage(nil), b...)
	return nil
}

// QueuedServiceConfig is the VM service block of a queued bare box. It is the
// shape the server's own podOfferRequestPayload builds.
type QueuedServiceConfig struct {
	Type              string `json:"type"` // "VMS", the orchestrator's ServiceTypeEnum.VM
	VMSlug            string `json:"vmSlug"`
	SSHKeyID          string `json:"sshKeyId"`
	SnapshotterConfig string `json:"snapshotterConfig"`
}

// QueuedDeployBody is the `request` of a queued deployment: a CreateDeployment
// body for a bare box. At fire time the server rewrites the offer identity
// (provider, desiredInstanceId, location_id, region) to the matched offer, so
// what is sent here only has to parse.
type QueuedDeployBody struct {
	Resource      ResourceSpec        `json:"resource"`
	Provider      ProviderSpec        `json:"provider"`
	ServiceConfig QueuedServiceConfig `json:"serviceConfig"`
}

// QueueDeploymentRequest is the body of POST /queued-deployments.
// PriceCeilingPerHour is mandatory on the wire: it is the spend rail for a
// create that fires with nobody watching.
type QueueDeploymentRequest struct {
	GPUModel            string           `json:"gpuModel"`
	GPUCount            int              `json:"gpuCount"`
	PriceCeilingPerHour float64          `json:"priceCeilingPerHour"`
	ProviderScope       string           `json:"providerScope,omitempty"`
	ExpiresInHours      *float64         `json:"expiresInHours,omitempty"`
	Request             QueuedDeployBody `json:"request"`
}

// QueueDeployment queues a deploy-when-available request.
func (c *Client) QueueDeployment(req QueueDeploymentRequest) (*QueuedDeployment, error) {
	var out QueuedDeployment
	if err := c.postJSON("/queued-deployments", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListQueuedDeployments lists the team's QUEUED rows.
func (c *Client) ListQueuedDeployments() ([]QueuedDeployment, error) {
	var out []QueuedDeployment
	if err := c.getJSON("/queued-deployments", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CancelQueuedDeployment cancels a queued request; a 404 means it is not active.
func (c *Client) CancelQueuedDeployment(id int) error {
	return c.deleteJSON("/queued-deployments/"+url.PathEscape(strconv.Itoa(id)), nil)
}
