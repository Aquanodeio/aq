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

// TestRunMovePostsOfferSelectionAndReportsTheNewPod checks `aq move` resolves
// the pod, builds an already-chosen offer from the marketplace (matching
// --provider), and hits POST /setups/:id/move with it nested under "offer",
// then prints the resulting environment/volume summary.
func TestRunMovePostsOfferSelectionAndReportsTheNewPod(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
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
	if !ok {
		t.Fatalf("body has no nested \"offer\" object: %#v", gotBody)
	}
	provider, ok := offer["provider"].(map[string]any)
	if !ok || provider["name"] != "runpod" {
		t.Errorf("offer.provider = %+v", provider)
	}
	if !strings.Contains(out.String(), "Moved trainer") {
		t.Errorf("expected a move confirmation; got:\n%s", out.String())
	}
}

// TestRunMoveSurfacesAFailedStartWithoutLosingData checks a failed Move
// (Stop succeeded, the new Start failed) surfaces an error that reassures
// the caller their data is intact, mechanism 7 of the pod/environment/
// volume plan.
func TestRunMoveSurfacesAFailedStartWithoutLosingData(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
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
