package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// TestRunJobRerunPostsToRerunRouteAndPrintsTheNewJob asserts runJobRerun
// resolves the source job by name, POSTs /jobs/:id/rerun (never mutating the
// source), and reports the NEW job's id/run — "running it again" always
// makes a new job (jobs-are-jobs spec).
func TestRunJobRerunPostsToRerunRouteAndPrintsTheNewJob(t *testing.T) {
	var hitRerun bool
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/rerun", func(w http.ResponseWriter, r *http.Request) {
		hitRerun = true
		if r.Method != http.MethodPost {
			t.Fatalf("want POST, got %s", r.Method)
		}
		writeData(w, map[string]any{
			"id":   "job-2",
			"name": "myjob-2",
			"run":  map[string]any{"id": "run-2", "status": "queued"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out bytes.Buffer
	opts := jobRerunOptions{
		cred:   &config.Credential{Token: "t", TeamID: "team-1", APIURL: srv.URL},
		target: "myjob",
		out:    &out,
	}
	if err := runJobRerun(opts); err != nil {
		t.Fatalf("runJobRerun: %v", err)
	}
	if !hitRerun {
		t.Fatal("want POST /jobs/:id/rerun hit")
	}
	if !strings.Contains(out.String(), "myjob-2") || !strings.Contains(out.String(), "run-2") {
		t.Fatalf("want the new job's name and run id printed, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "aq job logs myjob-2") {
		t.Fatalf("want a hint to `aq job logs` the new job, got: %s", out.String())
	}
}

// TestJobRerunRequiresATarget is the usage-line refusal.
func TestJobRerunRequiresATarget(t *testing.T) {
	err := jobRerun(nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage: aq job rerun") {
		t.Fatalf("error should print usage, got: %v", err)
	}
}
