package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func i64Ptr(n int64) *int64 { return &n }

// TestPrintBatchJobsRendersGPUDurationAndCost pins the columns the
// jobs-are-jobs spec names for `aq job ls`: name, status, GPU, duration,
// cost. GPU and duration are derived from the run's own attempts/timestamps;
// cost is rendered straight from the server's own run.costCents, never
// computed here.
func TestPrintBatchJobsRendersGPUDurationAndCost(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	started := now.Add(-30 * time.Minute).Format(time.RFC3339)
	list := []api.BatchJob{
		{
			ID:   "job-1",
			Name: "train-run",
			Run: api.Run{
				ID:         "run-1",
				Status:     "running",
				StartedAt:  started,
				FinishedAt: "",
				CostCents:  i64Ptr(125),
				Attempts: []api.RunAttempt{
					{Ordinal: 1, GPUModel: strPtr("H100")},
				},
			},
		},
		{
			ID:   "job-2",
			Name: "no-attempt-yet",
			Run:  api.Run{ID: "run-2", Status: "queued"},
		},
	}

	var out bytes.Buffer
	printBatchJobs(&out, list, now)
	got := out.String()

	if !strings.Contains(got, "train-run") || !strings.Contains(got, "Running") || !strings.Contains(got, "H100") {
		t.Fatalf("want name/status/GPU rendered for the running job, got:\n%s", got)
	}
	if !strings.Contains(got, "30m0s") {
		t.Fatalf("want a 30m duration for the running job, got:\n%s", got)
	}
	if !strings.Contains(got, "$1.25") {
		t.Fatalf("want run.costCents (125) rendered as $1.25, got:\n%s", got)
	}
	if !strings.Contains(got, "no-attempt-yet") || !strings.Contains(got, "Starting") {
		t.Fatalf("want the queued job's name and \"Starting\" status rendered, got:\n%s", got)
	}
	// The queued job's run.costCents is nil -- must render "-", never "$0.00".
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		if strings.Contains(line, "no-attempt-yet") && !strings.HasSuffix(strings.TrimRight(line, " "), "-") {
			t.Fatalf("want nil costCents rendered as -, got line: %q", line)
		}
	}
}

// TestPrintBatchJobsUnservableRendersAsCouldNotRun: per the "failed means
// YOUR code failed" addendum, "unservable" must read as "Couldn't run", and
// the table must carry the one-line legend explaining that this was not the
// owner's own code.
func TestPrintBatchJobsUnservableRendersAsCouldNotRun(t *testing.T) {
	list := []api.BatchJob{{ID: "job-1", Name: "x", Run: api.Run{ID: "run-1", Status: "unservable"}}}
	var out bytes.Buffer
	printBatchJobs(&out, list, time.Now())
	got := out.String()
	if !strings.Contains(got, "Couldn't run") {
		t.Fatalf("want \"Couldn't run\" rendered, got: %s", got)
	}
	if !strings.Contains(got, couldNotRunLegend) {
		t.Fatalf("want the legend line printed when a row couldn't run, got: %s", got)
	}
}

// TestPrintBatchJobsLegendOnlyWhenARowCouldntRun: the legend must never
// appear when no displayed row is "Couldn't run" -- not even a blank line.
func TestPrintBatchJobsLegendOnlyWhenARowCouldntRun(t *testing.T) {
	list := []api.BatchJob{{ID: "job-1", Name: "x", Run: api.Run{ID: "run-1", Status: "succeeded"}}}
	var out bytes.Buffer
	printBatchJobs(&out, list, time.Now())
	if strings.Contains(out.String(), couldNotRunLegend) {
		t.Fatalf("did not want the legend line when no row couldn't run, got: %s", out.String())
	}
}

// TestJobStatusWordMapsEveryWireStatus pins the addendum's full mapping,
// table-driven, all 8 wire values.
func TestJobStatusWordMapsEveryWireStatus(t *testing.T) {
	cases := map[string]string{
		"queued":       "Starting",
		"placing":      "Starting",
		"provisioning": "Starting",
		"restoring":    "Starting",
		"running":      "Running",
		"uploading":    "Saving outputs",
		"succeeded":    "Succeeded",
		"failed":       "Failed",
		"cancelled":    "Cancelled",
		"unservable":   "Couldn't run",
	}
	for wire, want := range cases {
		if got := jobStatusWord(wire); got != want {
			t.Errorf("jobStatusWord(%q) = %q, want %q", wire, got, want)
		}
	}
}

// TestJobStatusWordPassesThroughUnknown: a status this CLI has never seen
// must never be guessed at -- render it verbatim rather than mislabeling it.
func TestJobStatusWordPassesThroughUnknown(t *testing.T) {
	if got := jobStatusWord("some-future-status"); got != "some-future-status" {
		t.Fatalf("jobStatusWord(unknown) = %q, want passthrough", got)
	}
}

// TestPrintBatchJobsEmptyListNudgesTowardJobRun: an empty list must not just
// print a blank table.
func TestPrintBatchJobsEmptyListNudgesTowardJobRun(t *testing.T) {
	var out bytes.Buffer
	printBatchJobs(&out, nil, time.Now())
	if !strings.Contains(out.String(), "aq job run") {
		t.Fatalf("want a nudge toward `aq job run`, got: %s", out.String())
	}
}

// TestFormatRunGPUNoAttemptsIsDash: a run that never landed a box (queued,
// or unservable) must render "-", never an empty string or a guess.
func TestFormatRunGPUNoAttemptsIsDash(t *testing.T) {
	if got := formatRunGPU(api.Run{}); got != "-" {
		t.Fatalf("formatRunGPU(no attempts) = %q, want -", got)
	}
}

// TestFormatRunDurationNeverStartedIsDash.
func TestFormatRunDurationNeverStartedIsDash(t *testing.T) {
	if got := formatRunDuration(api.Run{}, time.Now()); got != "-" {
		t.Fatalf("formatRunDuration(never started) = %q, want -", got)
	}
}

// TestFormatRunCostTableDriven pins the cost-cell logic the addendum spells
// out: null always "-" regardless of GPU model; a real zero renders "<$0.01"
// only when a GPU model was actually recorded (proof a box really ran);
// zero with no recorded GPU model falls back to "-" rather than asserting a
// figure this CLI cannot back up; a normal positive cost formats as today.
func TestFormatRunCostTableDriven(t *testing.T) {
	withGPU := []api.RunAttempt{{Ordinal: 1, GPUModel: strPtr("H100")}}
	noGPU := []api.RunAttempt{{Ordinal: 1, GPUModel: nil}}

	cases := []struct {
		name string
		run  api.Run
		want string
	}{
		{"nil costCents, no attempts", api.Run{CostCents: nil}, "-"},
		{"nil costCents, with GPU model", api.Run{CostCents: nil, Attempts: withGPU}, "-"},
		{"zero cost with GPU model", api.Run{CostCents: i64Ptr(0), Attempts: withGPU}, "<$0.01"},
		{"zero cost with no GPU model", api.Run{CostCents: i64Ptr(0), Attempts: noGPU}, "-"},
		{"zero cost with no attempts at all", api.Run{CostCents: i64Ptr(0)}, "-"},
		{"positive cost", api.Run{CostCents: i64Ptr(1234), Attempts: withGPU}, "$12.34"},
		{"positive cost with no attempts", api.Run{CostCents: i64Ptr(150)}, "$1.50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatRunCost(tc.run); got != tc.want {
				t.Fatalf("formatRunCost(%+v) = %q, want %q", tc.run, got, tc.want)
			}
		})
	}
}

// TestJobLsRequestsBatchShape asserts the actual query string GET /jobs
// receives, never just that ListBatchJobs' Go signature was called with
// "batch" — the wire is the contract, mirroring
// TestEndpointListRequestsServiceShape's own reasoning for the other shape.
func TestJobLsRequestsBatchShape(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeData(w, []map[string]any{})
	}))
	defer srv.Close()

	client := newControlClient(&config.Credential{Token: "t", TeamID: "team-1", APIURL: srv.URL})
	if _, err := client.ListBatchJobs(); err != nil {
		t.Fatalf("ListBatchJobs: %v", err)
	}
	if gotQuery != "shape=batch" {
		t.Fatalf("query = %q, want shape=batch", gotQuery)
	}
}
