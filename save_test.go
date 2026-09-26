package main

import (
	"strings"
	"testing"
)

// TestSnapshotRefusesAManagedTarget pins the post-migration shape of `aq
// save`: it used to also checkpoint a managed pod (POST /setups/:id/snapshot,
// now gone under the pod/environment/volume model, see stop.go), and must now
// refuse a non-host target with a message pointing at `aq stop` instead of
// silently doing nothing or erroring in a confusing way.
func TestSnapshotRefusesAManagedTarget(t *testing.T) {
	err := snapshot([]string{"comfyui"})
	if err == nil || !strings.Contains(err.Error(), "aq stop") {
		t.Fatalf("expected a refusal pointing at `aq stop`, got: %v", err)
	}
}

func TestSnapshotRequiresATarget(t *testing.T) {
	err := snapshot(nil)
	if err == nil || !strings.Contains(err.Error(), "usage: aq save") {
		t.Fatalf("expected a usage error, got: %v", err)
	}
}
