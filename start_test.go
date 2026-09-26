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

// TestRunStartResolvesPodByNameAndSendsOfferFilters checks `aq start` resolves
// a pod by name (not just uuid), posts the GPU filters NESTED under "offer",
// and prints the returned environment/volume summary.
func TestRunStartResolvesPodByNameAndSendsOfferFilters(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "11111111-2222-3333-4444-555555555555", "name": "trainer"},
		})
	})
	var gotPath string
	var gotBody map[string]any
	mux.HandleFunc("/setups/11111111-2222-3333-4444-555555555555/start", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeData(w, map[string]any{
			"id": "11111111-2222-3333-4444-555555555555", "name": "trainer",
			"environment": map[string]any{"id": "e1", "name": "pytorch-dev", "version": 3, "kind": "kept"},
			"volume":      map[string]any{"id": "v1", "name": "research-data", "sizeBytes": 1073741824, "saveState": "saved"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runStart(startOptions{cred: cred, target: "trainer", gpuModel: "RTX 4090", maxPrice: 1.2, out: &out})
	if err != nil {
		t.Fatalf("runStart: %v", err)
	}
	if gotPath == "" {
		t.Fatal("start route was never called")
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok || offer["gpuModel"] != "RTX 4090" {
		t.Errorf("offer body = %#v", gotBody)
	}

	got := out.String()
	for _, want := range []string{"Starting trainer", "Environment: pytorch-dev", "Volume: research-data", "saved"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// TestRunStartOmitsVolumeLineForABarePod checks a pod with no volume prints
// only the environment line, never a blank/zeroed "Volume:" row.
func TestRunStartOmitsVolumeLineForABarePod(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "bare"}})
	})
	mux.HandleFunc("/setups/pod-1/start", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"id": "pod-1", "name": "bare",
			"environment": map[string]any{"id": "e1", "name": "torch_and_jupyter", "version": nil, "kind": "builtin"},
			"volume":      nil,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStart(startOptions{cred: cred, target: "bare", out: &out}); err != nil {
		t.Fatalf("runStart: %v", err)
	}
	if strings.Contains(out.String(), "Volume:") {
		t.Errorf("expected no Volume line for a bare pod; got:\n%s", out.String())
	}
}
