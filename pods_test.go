package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// TestFormatPodEnvironmentRendersNameAndVersion checks the "name vN" shape
// for a pod whose environment has a minted version, and bare "name" (D15
// vocabulary allowance: "version" is fine on environment versions) when
// Version is nil, the state before a pod's environment is ever Kept or
// Shared.
func TestFormatPodEnvironmentRendersNameAndVersion(t *testing.T) {
	three := 3
	cases := []struct {
		name string
		env  api.SetupEnvironmentSummary
		want string
	}{
		{"versioned", api.SetupEnvironmentSummary{Name: "pytorch-dev", Version: &three}, "pytorch-dev v3"},
		{"unversioned", api.SetupEnvironmentSummary{Name: "torch_and_jupyter", Version: nil}, "torch_and_jupyter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatPodEnvironment(tc.env); got != tc.want {
				t.Errorf("formatPodEnvironment(%+v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// TestPrintPodsRendersTheEnvironmentColumn checks `aq pods`' table header
// says ENVIRONMENT (not the old VERSION-only column keyed off the legacy
// SnapshotVersion lineage) and each row shows the pod's own
// Setup.environment {name, version}, straight off the GET /setups row with
// no separate ListAllSetupVersions lookup.
func TestPrintPodsRendersTheEnvironmentColumn(t *testing.T) {
	four := 4
	list := []api.Setup{
		{
			ID: "pod-1", Name: "trainer", LeaseDeploymentID: intPtr(42),
			Environment: api.SetupEnvironmentSummary{Name: "pytorch-dev", Version: &four},
		},
		{
			ID: "pod-2", Name: "bare",
			Environment: api.SetupEnvironmentSummary{Name: "torch_and_jupyter", Version: nil},
		},
	}

	var out bytes.Buffer
	printPods(&out, list)
	got := out.String()

	if !strings.Contains(got, "ENVIRONMENT") {
		t.Errorf("header must name the ENVIRONMENT column; got:\n%s", got)
	}
	if strings.Contains(got, "VERSION") {
		t.Errorf("header must not still say VERSION; got:\n%s", got)
	}
	if !strings.Contains(got, "trainer") || !strings.Contains(got, "pytorch-dev v4") {
		t.Errorf("expected trainer's row to show \"pytorch-dev v4\"; got:\n%s", got)
	}
	if !strings.Contains(got, "bare") || !strings.Contains(got, "torch_and_jupyter") {
		t.Errorf("expected bare's row to show its unversioned environment name; got:\n%s", got)
	}
}

// TestPrintPodsNudgesWhenEmpty checks a caller with no pods gets pointed at
// `aq up` instead of an empty table.
func TestPrintPodsNudgesWhenEmpty(t *testing.T) {
	var out bytes.Buffer
	printPods(&out, nil)
	if !strings.Contains(out.String(), "aq up") {
		t.Errorf("expected a nudge toward `aq up`; got:\n%s", out.String())
	}
}

func intPtr(n int) *int { return &n }
