package api

import "strconv"

// Idle-auto-stop endpoints used by `aq idle status` / `aq idle set`.

// IdlePolicy mirrors GET /deployments/:deploymentId/idle-policy: the
// deployment's merged idle-auto-stop policy plus a live verdict computed from
// its current usage data.
type IdlePolicy struct {
	WarnAfterMinutes        int  `json:"warnAfterMinutes"`
	ActAfterMinutes         int  `json:"actAfterMinutes"`
	GPUIdleThresholdPercent int  `json:"gpuIdleThresholdPercent"`
	AutoStopEnabled         bool `json:"autoStopEnabled"`
	// State is one of UNKNOWN | ACTIVE | IDLE_WARN | IDLE_ACT. UNKNOWN means the
	// box has reported no usable usage data yet — render it as unknown, never as
	// active or idle: presenting a no-data verdict as either extreme is a lie the
	// user could act on.
	State string `json:"state"`
	// IdleMinutes is only meaningful once State is IDLE_WARN or IDLE_ACT.
	IdleMinutes int `json:"idleMinutes"`
}

// GetIdlePolicy fetches a deployment's merged idle-auto-stop policy and its
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
type IdlePolicyUpdate struct {
	WarnAfterMinutes        *int  `json:"warnAfterMinutes,omitempty"`
	ActAfterMinutes         *int  `json:"actAfterMinutes,omitempty"`
	GPUIdleThresholdPercent *int  `json:"gpuIdleThresholdPercent,omitempty"`
	AutoStopEnabled         *bool `json:"autoStopEnabled,omitempty"`
}

// SetIdlePolicy updates a deployment's idle-auto-stop policy and returns the
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
