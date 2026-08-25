package api

import (
	"encoding/json"
	"testing"
)

// TestIdlePolicyDecodesNewAutoPauseField checks the common case: a backend
// that has shipped the #753 rename sends "autoPauseEnabled", and it decodes
// straight through.
func TestIdlePolicyDecodesNewAutoPauseField(t *testing.T) {
	var p IdlePolicy
	body := `{"warnAfterMinutes":30,"actAfterMinutes":60,"gpuIdleThresholdPercent":5,"autoPauseEnabled":true,"state":"ACTIVE","idleMinutes":0}`
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !p.AutoPauseEnabled {
		t.Errorf("AutoPauseEnabled = false, want true")
	}
}

// TestIdlePolicyFallsBackToOldAutoStopField checks that a backend running
// ahead of the #753 rename (still emitting the pre-rename "autoStopEnabled"
// and never "autoPauseEnabled") still decodes correctly — a released `aq`
// binary talks to whatever backend is deployed, and must not break against
// one that predates this rename.
func TestIdlePolicyFallsBackToOldAutoStopField(t *testing.T) {
	var p IdlePolicy
	body := `{"warnAfterMinutes":30,"actAfterMinutes":60,"gpuIdleThresholdPercent":5,"autoStopEnabled":true,"state":"ACTIVE","idleMinutes":0}`
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !p.AutoPauseEnabled {
		t.Errorf("AutoPauseEnabled = false, want true (fallback from autoStopEnabled)")
	}
}

// TestIdlePolicyPrefersNewFieldWhenBothPresent checks the transition window
// where a backend emits both names for one release (per the general #753
// ticket, matching IdlePolicyUpdateSchema's dual-accept on input) — the new
// name must win, never the old one.
func TestIdlePolicyPrefersNewFieldWhenBothPresent(t *testing.T) {
	var p IdlePolicy
	body := `{"warnAfterMinutes":30,"actAfterMinutes":60,"gpuIdleThresholdPercent":5,"autoPauseEnabled":true,"autoStopEnabled":false,"state":"ACTIVE","idleMinutes":0}`
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !p.AutoPauseEnabled {
		t.Errorf("AutoPauseEnabled = false, want true (new field must win over stale old one)")
	}
}
