package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// TestFormatPodEnvironmentRendersNameAndVersion checks the "name vN" shape
// for a pod whose environment has a minted version, bare "name" (D15
// vocabulary allowance: "version" is fine on environment versions) when
// Version is nil, and "unknown" (never a bare "-") for a nil Environment
// itself: the rare row whose environmentVersionId didn't resolve
// (SetupEnvironmentSummary's doc comment). Collapsing that into an
// empty-but-rendered name is exactly the "environment version -" bug this
// pins against.
func TestFormatPodEnvironmentRendersNameAndVersion(t *testing.T) {
	three := 3
	cases := []struct {
		name string
		env  *api.SetupEnvironmentSummary
		want string
	}{
		{"versioned", &api.SetupEnvironmentSummary{Name: "pytorch-dev", Version: &three}, "pytorch-dev v3"},
		{"unversioned", &api.SetupEnvironmentSummary{Name: "torch_and_jupyter", Version: nil}, "torch_and_jupyter"},
		{"nil environment", nil, "unknown"},
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
			ID: "pod-1", Name: "trainer", AttachedDeploymentID: intPtr(42),
			Environment: &api.SetupEnvironmentSummary{Name: "pytorch-dev", Version: &four},
		},
		{
			ID: "pod-2", Name: "bare",
			Environment: &api.SetupEnvironmentSummary{Name: "torch_and_jupyter", Version: nil},
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

// TestPrintPodsRunningStateAndSizeMatchTheWire is the console-disagreement
// regression: a running pod (attachedDeploymentId set) must read "yes", a
// bare pod with no volume must read "nothing held" (never the same "-" an
// unmeasured volume uses), and an attached-but-unmeasured volume must read
// "not measured yet" rather than "0 B" or "-": three states a single dash
// used to collapse into one.
func TestPrintPodsRunningStateAndSizeMatchTheWire(t *testing.T) {
	list := []api.Setup{
		{
			ID: "pod-1", Name: "running-pod", AttachedDeploymentID: intPtr(99),
			Environment: &api.SetupEnvironmentSummary{Name: "Ubuntu 24.04 + CUDA", Kind: "builtin"},
			Volume:      &api.SetupVolumeSummary{Name: "running-pod", SizeBytes: int64Ptr(8288931), SaveState: "saved"},
		},
		{
			ID: "pod-2", Name: "bare-pod",
			Environment: &api.SetupEnvironmentSummary{Name: "Ubuntu 24.04 + CUDA", Kind: "builtin"},
			Volume:      nil,
		},
		{
			ID: "pod-3", Name: "unmeasured-pod",
			Environment: &api.SetupEnvironmentSummary{Name: "Ubuntu 24.04 + CUDA", Kind: "builtin"},
			Volume:      &api.SetupVolumeSummary{Name: "unmeasured-pod", SizeBytes: nil, SaveState: "unknown"},
		},
	}

	var out bytes.Buffer
	printPods(&out, list)
	got := out.String()

	lineFor := func(name string) string {
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, name) {
				return line
			}
		}
		t.Fatalf("no row for %q; got:\n%s", name, got)
		return ""
	}

	running := lineFor("running-pod")
	if !strings.Contains(running, "yes") {
		t.Errorf("running pod (attachedDeploymentId set) must read RUNNING=yes; got line: %q", running)
	}

	bare := lineFor("bare-pod")
	if !strings.Contains(bare, "no") || !strings.Contains(bare, "nothing held") {
		t.Errorf("bare pod (no volume) must read RUNNING=no and SIZE=\"nothing held\"; got line: %q", bare)
	}

	unmeasured := lineFor("unmeasured-pod")
	if !strings.Contains(unmeasured, "not measured yet") {
		t.Errorf("unmeasured volume must read \"not measured yet\", never \"0 B\" or \"-\"; got line: %q", unmeasured)
	}
	if strings.Contains(unmeasured, "0 B") {
		t.Errorf("an unmeasured volume must never render as \"0 B\" (that means genuinely empty); got line: %q", unmeasured)
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

func int64Ptr(n int64) *int64 { return &n }

func strPtr(s string) *string { return &s }
