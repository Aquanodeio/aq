package api

import "net/url"

// Pod lifecycle endpoints for the pod/environment/volume model: Start, Stop,
// Move, and the per-pod autostop preference. These replace the old
// pause/resume cycle (`aq pause` saved and released a setup's lease; resuming
// meant `aq deploy --snapshot <deploymentId>` naming a raw deployment id).
// Under this model a pod is Running or Stopped, nothing else: Stop always
// saves (both the environment and the volume, confirmed) before releasing
// the box, and Start brings it back on ANY matching GPU, not necessarily the
// one it last ran on.

// OfferFilter narrows the marketplace offer Start/Move rent against. Every
// field is optional (omitempty) — an absent field means "no opinion", not
// "match nothing": the orchestrator picks the cheapest offer that satisfies
// whatever IS set, exactly like `aq up`/`aq deploy`'s flattened filter
// fields (control.go's UpRequest/DeployRequest), just nested here under
// "offer" per the pod/environment/volume plan's wire contract.
type OfferFilter struct {
	GPUModel string  `json:"gpuModel,omitempty"`
	MaxPrice float64 `json:"maxPrice,omitempty"`
	Provider string  `json:"provider,omitempty"`
	// GPUCount asks for a multi-GPU box; 0 means "no opinion" (the
	// orchestrator's own default of one). See UpRequest.GPUCount.
	GPUCount int `json:"gpuCount,omitempty"`
}

// StartSetupRequest is the body of POST /setups/:id/start.
type StartSetupRequest struct {
	Offer OfferFilter `json:"offer"`
}

// StartSetup starts a Stopped pod on the cheapest offer matching req.Offer's
// filters (all empty = cheapest offer anywhere), returning the pod's updated
// state. Restoring the environment and the volume both gate readiness; the
// pod is Running only once both land (`ready_at`), never on the first
// reachable signal.
func (c *Client) StartSetup(setupID string, req StartSetupRequest) (*Setup, error) {
	var out Setup
	path := "/setups/" + url.PathEscape(setupID) + "/start"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StopSetup saves the pod (environment capture and volume sync, both
// confirmed) and releases its box. The pod's config and history are
// untouched — Stop never deletes anything; see StartSetup for bringing it
// back. A failed save keeps the box Running with the error reported on the
// pod, rather than releasing a box whose save just failed.
func (c *Client) StopSetup(setupID string) (*Setup, error) {
	var out Setup
	path := "/setups/" + url.PathEscape(setupID) + "/stop"
	if err := c.postJSON(path, struct{}{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MoveSetupRequest is the body of POST /setups/:id/move.
type MoveSetupRequest struct {
	Offer OfferFilter `json:"offer"`
}

// MoveSetup stops the pod (both captures confirmed, box released only after)
// then starts it again on the cheapest offer matching req.Offer's filters. A
// failed Start after the Stop leaves the pod Stopped with its data intact —
// see the pod/environment/volume plan's mechanism 7.
func (c *Client) MoveSetup(setupID string, req MoveSetupRequest) (*Setup, error) {
	var out Setup
	path := "/setups/" + url.PathEscape(setupID) + "/move"
	if err := c.postJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetupAutostopRequest is the body of PUT /setups/:id/autostop. Enabled is
// three-state on the wire (enabled:boolean|null — null clears back to the
// platform default). `aq autostop` itself only ever sends true or false
// today (there is no "unset" verb yet, matching the old `aq autopause`'s own
// limitation), but the pointer keeps the client honest about what the wire
// allows.
type SetupAutostopRequest struct {
	Enabled *bool `json:"enabled"`
}

// SetSetupAutostop replaces the old SetSetupAutopause: same mechanism (a
// per-pod stop-when-idle preference layered underneath `aq idle`'s
// per-deployment thresholds), renamed route and field per the
// pod/environment/volume plan's vocabulary sweep — "pause" is retired
// everywhere except environment versions (D15).
func (c *Client) SetSetupAutostop(setupID string, enabled bool) (*Setup, error) {
	var out Setup
	path := "/setups/" + url.PathEscape(setupID) + "/autostop"
	if err := c.putJSON(path, SetupAutostopRequest{Enabled: &enabled}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
