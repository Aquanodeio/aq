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

// TestRunMovePostsOfferFilterAndReportsTheNewPod checks `aq move` resolves
// the pod, hits POST /setups/:id/move with the GPU filters nested under
// "offer", and prints the resulting environment/volume summary.
func TestRunMovePostsOfferFilterAndReportsTheNewPod(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	var gotBody map[string]any
	mux.HandleFunc("/setups/pod-1/move", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeData(w, map[string]any{
			"id": "pod-1", "name": "trainer",
			"environment": map[string]any{"id": "e1", "name": "pytorch-dev", "version": 4, "kind": "kept"},
			"volume":      map[string]any{"id": "v1", "name": "research-data", "sizeBytes": 1073741824, "saveState": "saved"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runMove(moveOptions{cred: cred, target: "trainer", provider: "runpod", out: &out})
	if err != nil {
		t.Fatalf("runMove: %v", err)
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok || offer["provider"] != "runpod" {
		t.Errorf("offer body = %#v", gotBody)
	}
	if !strings.Contains(out.String(), "Moved trainer") {
		t.Errorf("expected a move confirmation; got:\n%s", out.String())
	}
}

// TestRunMoveSurfacesAFailedStartWithoutLosingData checks a failed Move
// (Stop succeeded, the new Start failed) surfaces an error that reassures
// the caller their data is intact — mechanism 7 of the pod/environment/
// volume plan.
func TestRunMoveSurfacesAFailedStartWithoutLosingData(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/move", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "no matching offer available"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runMove(moveOptions{cred: cred, target: "trainer", out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "data is safe") {
		t.Fatalf("expected an error reassuring the data is safe, got: %v", err)
	}
}
