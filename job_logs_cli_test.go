package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// TestJobLogsResolvesTheJobsOneRunAutomatically: `aq job logs <job>` takes a
// single positional now — a job is 1:1 with its Run under the jobs-are-jobs
// contract — so the run id must be
// resolved via ListRuns rather than typed as a second argument.
func TestJobLogsResolvesTheJobsOneRunAutomatically(t *testing.T) {
	var hitLogs bool
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/runs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "run-1", "status": "succeeded"}})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs", func(w http.ResponseWriter, r *http.Request) {
		hitLogs = true
		writeData(w, map[string]any{"chunk": "hi\n", "nextOffset": 3, "size": 3, "truncated": false, "source": "archived"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	detachedSandbox(t)
	if err := config.Save(&config.Credential{Token: "t", TeamID: "team-1", APIURL: srv.URL}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if err := jobLogs([]string{"myjob"}); err != nil {
		t.Fatalf("jobLogs: %v", err)
	}
	if !hitLogs {
		t.Fatal("want the resolved run's log endpoint hit")
	}
}

// TestJobLogsRejectsTwoPositionals: the old `<job> <run-id>` two-argument
// form is gone, not merely optional — a stray second argument must be
// refused with the new usage line, never silently ignored.
func TestJobLogsRejectsTwoPositionals(t *testing.T) {
	err := jobLogs([]string{"myjob", "run-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage: aq job logs <job>") {
		t.Fatalf("error should print the new one-positional usage, got: %v", err)
	}
}

// TestJobCancelResolvesTheJobsOneRunAutomatically mirrors the logs test
// above for `aq job cancel <job>`.
func TestJobCancelResolvesTheJobsOneRunAutomatically(t *testing.T) {
	var cancelledRunID string
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/runs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "run-1", "status": "running"}})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/cancel", func(w http.ResponseWriter, r *http.Request) {
		cancelledRunID = "run-1"
		writeData(w, map[string]any{"id": "run-1", "status": "cancelled"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	detachedSandbox(t)
	if err := config.Save(&config.Credential{Token: "t", TeamID: "team-1", APIURL: srv.URL}); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	if err := jobCancel([]string{"myjob"}); err != nil {
		t.Fatalf("jobCancel: %v", err)
	}
	if cancelledRunID != "run-1" {
		t.Fatalf("want run-1 cancelled, got %q", cancelledRunID)
	}
}

// TestJobCancelRejectsARunIDArgument: the old `<job> <run-id>` form is gone.
func TestJobCancelRejectsARunIDArgument(t *testing.T) {
	err := jobCancel([]string{"myjob", "run-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage: aq job cancel <job>") {
		t.Fatalf("error should print the new one-positional usage, got: %v", err)
	}
}
