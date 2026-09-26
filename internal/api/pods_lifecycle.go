package api

import (
	"encoding/json"
	"errors"
	"net/url"
)

// Pod lifecycle endpoints for the pod/environment/volume model: Create,
// Start, Stop, Move, and the per-pod autostop preference. These replace the
// old pause/resume cycle (`aq pause` saved and released a setup's lease;
// resuming meant `aq deploy --snapshot <deploymentId>` naming a raw
// deployment id). Under this model a pod is Running or Stopped, nothing
// else: Stop always saves (both the environment and the volume, confirmed)
// before releasing the box, and Start brings it back on ANY matching GPU,
// not necessarily the one it last ran on.

// ResourceSpec is the `resource` object inside an OfferSelection, mirroring
// the orchestrator's own ResourceSchema (deployment.schemas.ts) byte for
// byte — the pod/environment/volume plan's REST amendment (2026-09-26)
// states this is the SAME shape POST /deployments/deploy already takes, not
// a new one invented for pods. CPU/Memory/Storage are REQUIRED on the wire
// (no omitempty): the schema has no default for them, and a caller must
// resolve a real number/string from the chosen marketplace offer rather than
// omit the key.
//
// DesiredInstanceID is the SERVER's own name for what a marketplace offer's
// own `address` field already is (console: `desiredInstanceId:
// chosenOffer.address` — aquanode-backend gotcha 1: "format:
// <provider_address>/<preset_id>"). It is `.optional()` on the zod schema
// but effectively REQUIRED by createDeployment (400s without it), so a
// caller building this from a real offer must always send it.
type ResourceSpec struct {
	CPU               int    `json:"cpu"`
	Memory            string `json:"memory"`
	Storage           string `json:"storage"`
	GPUUnits          int    `json:"gpuUnits,omitempty"`
	GPUModel          string `json:"gpuModel,omitempty"`
	DesiredInstanceID string `json:"desiredInstanceId,omitempty"`
	Region            string `json:"region,omitempty"`
	LocationID        string `json:"location_id,omitempty"`
}

// ProviderSpec is the `provider` object inside an OfferSelection.
type ProviderSpec struct {
	Name string `json:"name"`
}

// OfferSelection is the `offer` object POST /setups, /setups/:id/start, and
// /setups/:id/move all take (the pod/environment/volume plan's REST
// amendment, 2026-09-26): a single, ALREADY-CHOSEN marketplace offer, not a
// filter for the orchestrator to resolve server-side the way
// UpRequest/DeployRequest's flattened gpuModel/maxPrice/provider fields are.
// The caller (aq's start.go/move.go) is responsible for querying the
// marketplace, picking the cheapest offer matching its own filters, and
// building this from it. Image, ports, startup script, and template are NOT
// here — they come from the pod's own config columns (D10).
type OfferSelection struct {
	Resource ResourceSpec `json:"resource"`
	Provider ProviderSpec `json:"provider"`
	SSHKeyID string       `json:"sshKeyId"`
}

// CreateSetupRequest is the body of POST /setups (New pod): create the pod
// and start it on Offer in one call (confirmed against createSetupSchema,
// orchestrator/src/schemas/setups.schemas.ts, 2026-09-26).
//
// Name is omitted (never sent empty) when the caller has none; the server
// derives one from the environment's template or the GPU.
//
// VolumeID is ALWAYS present on the wire, never omitted: `"new"` mints a
// fresh volume named after the pod, an existing volume's id attaches it,
// and a nil pointer serializes to JSON `null` and runs the pod with no
// volume at all (D4). The schema is explicit about this ("never defaulted
// server-side"), so this field has no `omitempty` even though it's a
// pointer.
type CreateSetupRequest struct {
	Name                 string         `json:"name,omitempty"`
	Offer                OfferSelection `json:"offer"`
	EnvironmentVersionID string         `json:"environmentVersionId"`
	VolumeID             *string        `json:"volumeId"`
}

// CreateSetupRefusedStart is returned by CreateSetup when POST /setups
// creates the pod but the immediate Start it also attempts is refused (a
// bad SSH key, the chosen offer gone, insufficient credits, ...). The pod
// is real: created, Stopped, and already listed by `aq pods`. Setup carries
// its state as of the refusal. Never retry the create on this error (a
// second call mints a second pod); `aq start` the one that already exists
// once the refusal reason is fixed.
type CreateSetupRefusedStart struct {
	Setup *Setup
	Err   error
}

func (e *CreateSetupRefusedStart) Error() string { return e.Err.Error() }
func (e *CreateSetupRefusedStart) Unwrap() error { return e.Err }

// CreateSetup creates a new pod and starts it on the given offer in one
// call, POST /setups. See CreateSetupRefusedStart for the one error shape
// this unwraps specially; every other failure (a validation 400, a name
// conflict 409, ...) surfaces as a plain *APIError with no pod created.
func (c *Client) CreateSetup(req CreateSetupRequest) (*Setup, error) {
	var out Setup
	if err := c.postJSON("/setups", req, &out); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && len(apiErr.Data) > 0 {
			var body struct {
				Setup *Setup `json:"setup"`
			}
			if jsonErr := json.Unmarshal(apiErr.Data, &body); jsonErr == nil && body.Setup != nil {
				return nil, &CreateSetupRefusedStart{Setup: body.Setup, Err: apiErr}
			}
		}
		return nil, err
	}
	return &out, nil
}

// StartSetupRequest is the body of POST /setups/:id/start.
type StartSetupRequest struct {
	Offer OfferSelection `json:"offer"`
}

// StartSetup starts a Stopped pod on the given offer, returning the pod's
// updated state. Restoring the environment and the volume both gate
// readiness; the pod is Running only once both land (`ready_at`), never on
// the first reachable signal.
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
	Offer OfferSelection `json:"offer"`
}

// MoveSetup stops the pod (both captures confirmed, box released only after)
// then starts it again on the given offer. A failed Start after the Stop
// leaves the pod Stopped with its data intact — see the pod/environment/
// volume plan's mechanism 7.
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
