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

// stubMarketplaceOffer registers a single-offer /marketplace fixture, for
// tests that exercise buildOfferSelection's client-side cheapest-offer pick
// (start.go/move.go) rather than posting a filter for the orchestrator to
// resolve.
func stubMarketplaceOffer(mux *http.ServeMux, gpuModel, provider string) {
	mux.HandleFunc("/marketplace", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{
				"address":          provider + "/offer-1",
				"gpuCount":         1,
				"gpuShortName":     gpuModel,
				"availableCpu":     8,
				"availableMemory":  map[string]any{"value": 32, "unit": "GB"},
				"availableStorage": map[string]any{"value": 200, "unit": "GB"},
				"price":            1.0,
				"region":           "US-EAST-1",
				"provider":         provider,
			},
		})
	})
}

// stubSSHKeys registers /settings/ssh-keys with no existing keys, so
// ensureSSHKey registers the sandboxed test HOME's local key and every
// Start/Move test exercises the same real path a fresh laptop takes.
func stubSSHKeys(mux *http.ServeMux) {
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeData(w, []map[string]any{})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeData(w, map[string]any{
			"id":         "key-new",
			"name":       body["name"],
			"public_key": body["public_key"],
		})
	})
}

// TestRunStartResolvesPodByNameAndSendsOfferSelection checks `aq start`
// resolves a pod by name (not just uuid), builds an already-chosen offer
// from the marketplace (matching --gpu) and posts it NESTED under "offer",
// then prints the returned environment/volume summary.
func TestRunStartResolvesPodByNameAndSendsOfferSelection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "11111111-2222-3333-4444-555555555555", "name": "trainer"},
		})
	})
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
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
	if !ok {
		t.Fatalf("body has no nested \"offer\" object: %#v", gotBody)
	}
	resource, ok := offer["resource"].(map[string]any)
	if !ok || resource["gpuModel"] != "RTX 4090" {
		t.Errorf("offer.resource = %+v", resource)
	}
	provider, ok := offer["provider"].(map[string]any)
	if !ok || provider["name"] != "runpod" {
		t.Errorf("offer.provider = %+v", provider)
	}
	if offer["sshKeyId"] == "" || offer["sshKeyId"] == nil {
		t.Errorf("offer.sshKeyId must be set, got %+v", offer)
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
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
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
