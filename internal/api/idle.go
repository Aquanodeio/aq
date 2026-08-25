package api

import (
	"encoding/json"
	"strconv"
)

// Idle-auto-pause endpoints used by `aq idle status` / `aq idle set`.

// IdlePolicy mirrors GET /deployments/:deploymentId/idle-policy: the
// deployment's merged idle-auto-pause policy plus a live verdict computed
// from its current usage data.
type IdlePolicy struct {
	WarnAfterMinutes        int `json:"warnAfterMinutes"`
	ActAfterMinutes         int `json:"actAfterMinutes"`
	GPUIdleThresholdPercent int `json:"gpuIdleThresholdPercent"`
	// AutoPauseEnabled is the renamed successor to the orchestrator's
	// "autoStopEnabled" field (meta-repo ticket #753 — "pause" is the
	// product's one noun for save-then-release, "auto-stop" is retired). It
	// is decoded manually below rather than via a struct tag: a released
	// `aq` binary talks to whatever backend happens to be deployed, and a
	// backend that predates the rename only ever sends the old name, so the
	// new name is tried first and the old name is a fallback, never the
	// reverse.
	AutoPauseEnabled bool `json:"-"`
	// State is one of UNKNOWN | ACTIVE | IDLE_WARN | IDLE_ACT. UNKNOWN means the
	// box has reported no usable usage data yet — render it as unknown, never as
	// active or idle: presenting a no-data verdict as either extreme is a lie the
	// user could act on.
	State string `json:"state"`
	// IdleMinutes is only meaningful once State is IDLE_WARN or IDLE_ACT.
	IdleMinutes int `json:"idleMinutes"`
}

// UnmarshalJSON decodes IdlePolicy, preferring the wire field
// "autoPauseEnabled" and falling back to the pre-rename "autoStopEnabled"
// when the new field is absent — see the AutoPauseEnabled doc comment above.
func (p *IdlePolicy) UnmarshalJSON(b []byte) error {
	type plain IdlePolicy
	var wire struct {
		plain
		AutoPauseEnabled *bool `json:"autoPauseEnabled"`
		AutoStopEnabled  *bool `json:"autoStopEnabled"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return err
	}
	*p = IdlePolicy(wire.plain)
	switch {
	case wire.AutoPauseEnabled != nil:
		p.AutoPauseEnabled = *wire.AutoPauseEnabled
	case wire.AutoStopEnabled != nil:
		p.AutoPauseEnabled = *wire.AutoStopEnabled
	}
	return nil
}

// GetIdlePolicy fetches a deployment's merged idle-auto-pause policy and its
// current live verdict.
func (c *Client) GetIdlePolicy(deploymentID int) (*IdlePolicy, error) {
	var out IdlePolicy
	path := "/deployments/" + strconv.Itoa(deploymentID) + "/idle-policy"
	if err := c.getJSON(path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// IdlePolicyUpdate is the body of PUT /deployments/:deploymentId/idle-policy.
// Every field is optional and a nil field is omitted from the request
// entirely (via `omitempty` on the pointer), so a flag the user never passed
// can never round-trip as a zero-value that silently overwrites one of their
// other settings.
//
// AutoPauseEnabled sends the renamed wire field: IdlePolicyUpdateSchema
// accepts both "autoPauseEnabled" and the pre-rename "autoStopEnabled" for
// one release, so this CLI always sends the new name — see the IdlePolicy
// doc comment above for why the response side stays tolerant of the old one.
type IdlePolicyUpdate struct {
	WarnAfterMinutes        *int  `json:"warnAfterMinutes,omitempty"`
	ActAfterMinutes         *int  `json:"actAfterMinutes,omitempty"`
	GPUIdleThresholdPercent *int  `json:"gpuIdleThresholdPercent,omitempty"`
	AutoPauseEnabled        *bool `json:"autoPauseEnabled,omitempty"`
}

// SetIdlePolicy updates a deployment's idle-auto-pause policy and returns the
// resulting merged policy plus live verdict. The orchestrator is the real
// authority on validity (e.g. warnAfterMinutes < actAfterMinutes) — this only
// shapes the request.
func (c *Client) SetIdlePolicy(deploymentID int, req IdlePolicyUpdate) (*IdlePolicy, error) {
	var out IdlePolicy
	path := "/deployments/" + strconv.Itoa(deploymentID) + "/idle-policy"
	if err := c.putJSON(path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
