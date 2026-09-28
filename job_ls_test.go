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

	if !strings.Contains(got, "train-run") || !strings.Contains(got, "running") || !strings.Contains(got, "H100") {
		t.Fatalf("want name/status/GPU rendered for the running job, got:\n%s", got)
	}
	if !strings.Contains(got, "30m0s") {
		t.Fatalf("want a 30m duration for the running job, got:\n%s", got)
	}
	if !strings.Contains(got, "$1.25") {
		t.Fatalf("want run.costCents (125) rendered as $1.25, got:\n%s", got)
	}
	if !strings.Contains(got, "no-attempt-yet") || !strings.Contains(got, "queued") {
		t.Fatalf("want the queued job's name and status rendered, got:\n%s", got)
	}
	// The queued job's run.costCents is nil -- must render "-", never "$0.00".
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		if strings.Contains(line, "no-attempt-yet") && !strings.HasSuffix(strings.TrimRight(line, " "), "-") {
			t.Fatalf("want nil costCents rendered as -, got line: %q", line)
		}
	}
}

// TestPrintBatchJobsUnservableIsUppercased mirrors printRuns/printEndpoints'
// own convention: "unservable" must read as visibly distinct, never blend
// into an ordinary status word.
func TestPrintBatchJobsUppercasesUnservable(t *testing.T) {
	list := []api.BatchJob{{ID: "job-1", Name: "x", Run: api.Run{ID: "run-1", Status: "unservable"}}}
	var out bytes.Buffer
	printBatchJobs(&out, list, time.Now())
	if !strings.Contains(out.String(), "UNSERVABLE") {
		t.Fatalf("want UNSERVABLE rendered, got: %s", out.String())
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

// TestFormatRunCostCentsNilIsDash: nil (UNKNOWN) must render "-", never
// "$0.00" -- this CLI never computes a cost itself, it only renders what
// the server's run.costCents sends.
func TestFormatRunCostCentsNilIsDash(t *testing.T) {
	if got := formatRunCostCents(nil); got != "-" {
		t.Fatalf("formatRunCostCents(nil) = %q, want -", got)
	}
}

// TestFormatRunCostCentsRendersTheServersFigureVerbatim.
func TestFormatRunCostCentsRendersTheServersFigureVerbatim(t *testing.T) {
	if got := formatRunCostCents(i64Ptr(0)); got != "$0.00" {
		t.Fatalf("formatRunCostCents(0) = %q, want $0.00 -- a real zero, distinct from nil/UNKNOWN", got)
	}
	if got := formatRunCostCents(i64Ptr(1234)); got != "$12.34" {
		t.Fatalf("formatRunCostCents(1234) = %q, want $12.34", got)
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
