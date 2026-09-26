package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// TestRunStopSavesThenPrintsRestartHint checks `aq stop` hits POST
// /setups/:id/stop and tells the user how to bring it back, unlike the old
// pause which pointed at a raw deployment id via `aq deploy --snapshot`.
func TestRunStopSavesThenPrintsRestartHint(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	var stopCalled bool
	mux.HandleFunc("/setups/pod-1/stop", func(w http.ResponseWriter, r *http.Request) {
		stopCalled = true
		writeData(w, map[string]any{
			"id": "pod-1", "name": "trainer",
			"environment": map[string]any{"id": "e1", "name": "pytorch-dev", "version": 4, "kind": "kept"},
			"volume":      map[string]any{"id": "v1", "name": "research-data", "sizeBytes": 2147483648, "saveState": "saved"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStop(stopOptions{cred: cred, target: "trainer", out: &out}); err != nil {
		t.Fatalf("runStop: %v", err)
	}
	if !stopCalled {
		t.Fatal("stop route was never called")
	}

	got := out.String()
	if !strings.Contains(got, "aq start trainer") {
		t.Errorf("expected a restart hint naming `aq start`; got:\n%s", got)
	}
	if strings.Contains(got, "aq deploy") || strings.Contains(got, "aq up") {
		t.Errorf("must not point at the old pause/resume verbs; got:\n%s", got)
	}
}

// TestRunStopSurfacesAFailedSave checks a failing Stop (the orchestrator
// keeps the box Running when either capture fails) surfaces as a real error
// rather than a false "stopped" message.
func TestRunStopSurfacesAFailedSave(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "environment capture failed, pod is still running"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runStop(stopOptions{cred: cred, target: "trainer", out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("expected the failed-save error surfaced, got: %v", err)
	}
}
