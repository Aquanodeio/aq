package main

import (
	"strings"
	"testing"
)

// TestSyncNowRefusesAManagedTarget pins the post-migration shape of
// `aq sync-now`: it used to also force a managed pod's sync tick (POST
// /setups/:id/sync, now gone: a running pod's volume ticks itself, see the
// pod/environment/volume plan's periodic save mechanism), and must now
// refuse a non-host target rather than silently doing nothing.
func TestSyncNowRefusesAManagedTarget(t *testing.T) {
	err := syncNow([]string{"comfyui"})
	if err == nil || !strings.Contains(err.Error(), "periodic tick") {
		t.Fatalf("expected a refusal naming the periodic tick, got: %v", err)
	}
}

func TestSyncNowRequiresATarget(t *testing.T) {
	err := syncNow(nil)
	if err == nil || !strings.Contains(err.Error(), "usage: aq sync-now") {
		t.Fatalf("expected a usage error, got: %v", err)
	}
}
