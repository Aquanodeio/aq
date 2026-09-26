package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// TestRunPodsCreateDefaultsToANewVolume checks the default (neither --volume
// nor --no-volume) resolves --env to its latest version and sends
// volumeId:"new", matching the console's New pod screen default.
func TestRunPodsCreateDefaultsToANewVolume(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"builtin": []map[string]any{
				{"id": "env-1", "name": "pytorch-dev", "kind": "builtin", "latestVersion": map[string]any{"id": "ver-3", "version": 3, "createdAt": "2026-09-20T00:00:00Z"}},
			},
			"yours":  []map[string]any{},
			"shared": []map[string]any{},
		})
	})
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
	var gotBody map[string]any
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeData(w, map[string]any{
			"id": "pod-1", "name": "trainer", "deploymentId": 4242,
			"environment": map[string]any{"id": "env-1", "name": "pytorch-dev", "version": 3, "kind": "builtin"},
			"volume":      map[string]any{"id": "vol-1", "name": "trainer", "sizeBytes": nil, "saveState": "unknown"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runPodsCreate(podsCreateOptions{cred: cred, name: "trainer", env: "pytorch-dev", out: &out})
	if err != nil {
		t.Fatalf("runPodsCreate: %v", err)
	}

	if gotBody["name"] != "trainer" {
		t.Errorf("body.name = %v, want trainer", gotBody["name"])
	}
	if gotBody["environmentVersionId"] != "ver-3" {
		t.Errorf("body.environmentVersionId = %v, want ver-3 (the resolved latest version's id)", gotBody["environmentVersionId"])
	}
	if gotBody["volumeId"] != "new" {
		t.Errorf("body.volumeId = %v, want \"new\" by default", gotBody["volumeId"])
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok {
		t.Fatalf("body has no nested \"offer\" object: %#v", gotBody)
	}
	if _, ok := offer["resource"].(map[string]any); !ok {
		t.Errorf("offer.resource missing: %+v", offer)
	}

	got := out.String()
	for _, want := range []string{"Created and starting trainer", "aq status 4242", "Environment: pytorch-dev"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// TestRunPodsCreateNoVolumeSendsNull checks --no-volume sends a JSON null,
// never an absent key or the string "null" (D4: a bare pod is allowed).
func TestRunPodsCreateNoVolumeSendsNull(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"builtin": []map[string]any{{"id": "env-1", "name": "pytorch-dev", "kind": "builtin", "latestVersion": map[string]any{"id": "ver-3", "version": 3, "createdAt": "2026-09-20T00:00:00Z"}}},
			"yours":   []map[string]any{},
			"shared":  []map[string]any{},
		})
	})
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
	var gotRaw []byte
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		gotRaw, _ = io.ReadAll(r.Body)
		writeData(w, map[string]any{
			"id": "pod-1", "name": "bare",
			"environment": map[string]any{"id": "env-1", "name": "pytorch-dev", "version": 3, "kind": "builtin"},
			"volume":      nil,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runPodsCreate(podsCreateOptions{cred: cred, name: "bare", env: "pytorch-dev", noVolume: true, out: &out}); err != nil {
		t.Fatalf("runPodsCreate: %v", err)
	}
	if !strings.Contains(string(gotRaw), `"volumeId":null`) {
		t.Errorf("expected a literal volumeId:null in the request body; got: %s", gotRaw)
	}
}

// TestRunPodsCreateResolvesAnExplicitVersion checks --version N looks up
// that version NUMBER against the environment's own history rather than
// trusting it as a row id directly.
func TestRunPodsCreateResolvesAnExplicitVersion(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"builtin": []map[string]any{},
			"yours":   []map[string]any{{"id": "env-2", "name": "comfyui", "kind": "kept", "latestVersion": map[string]any{"id": "ver-5", "version": 5, "createdAt": "2026-09-25T00:00:00Z"}}},
			"shared":  []map[string]any{},
		})
	})
	mux.HandleFunc("/environments/env-2/versions", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "ver-5", "version": 5, "createdAt": "2026-09-25T00:00:00Z"},
			{"id": "ver-2", "version": 2, "createdAt": "2026-09-10T00:00:00Z"},
		})
	})
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
	var gotBody map[string]any
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeData(w, map[string]any{
			"id": "pod-1", "name": "rollback",
			"environment": map[string]any{"id": "env-2", "name": "comfyui", "version": 2, "kind": "kept"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	if err := runPodsCreate(podsCreateOptions{cred: cred, name: "rollback", env: "comfyui", version: 2, noVolume: true, out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("runPodsCreate: %v", err)
	}
	if gotBody["environmentVersionId"] != "ver-2" {
		t.Errorf("environmentVersionId = %v, want ver-2 (version 2's row id, not the latest ver-5)", gotBody["environmentVersionId"])
	}
}

// TestRunPodsCreateSurfacesARefusedStartWithoutLosingThePod checks a
// create-then-start refusal (a bad SSH key, insufficient credits, the offer
// gone) reports the pod as created and Stopped rather than a bare failure,
// since retrying the create would mint a second pod.
func TestRunPodsCreateSurfacesARefusedStartWithoutLosingThePod(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"builtin": []map[string]any{{"id": "env-1", "name": "pytorch-dev", "kind": "builtin", "latestVersion": map[string]any{"id": "ver-3", "version": 3, "createdAt": "2026-09-20T00:00:00Z"}}},
			"yours":   []map[string]any{},
			"shared":  []map[string]any{},
		})
	})
	stubMarketplaceOffer(mux, "RTX 4090", "runpod")
	stubSSHKeys(mux)
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   "insufficient credits",
			"data": map[string]any{
				"setup": map[string]any{
					"id": "pod-1", "name": "trainer",
					"environment": map[string]any{"id": "env-1", "name": "pytorch-dev", "version": 3, "kind": "builtin"},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runPodsCreate(podsCreateOptions{cred: cred, name: "trainer", env: "pytorch-dev", noVolume: true, out: &bytes.Buffer{}})
	if err == nil {
		t.Fatal("want an error when the start is refused")
	}
	if !strings.Contains(err.Error(), "not leaked") || !strings.Contains(err.Error(), "aq start trainer") || !strings.Contains(err.Error(), "insufficient credits") {
		t.Errorf("expected the not-leaked reassurance naming the pod and the refusal reason; got: %v", err)
	}
}

// TestPodsCreateRejectsVolumeAndNoVolumeTogether pins the flag-parsing
// guard: --volume and --no-volume are mutually exclusive.
func TestPodsCreateRejectsVolumeAndNoVolumeTogether(t *testing.T) {
	err := podsCreate([]string{"trainer", "--env", "pytorch-dev", "--volume", "vol-1", "--no-volume"})
	if err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("expected a mutual-exclusion error, got: %v", err)
	}
}

// TestPodsCreateRequiresEnv pins that --env is mandatory.
func TestPodsCreateRequiresEnv(t *testing.T) {
	err := podsCreate([]string{"trainer"})
	if err == nil || !strings.Contains(err.Error(), "--env is required") {
		t.Fatalf("expected a missing --env error, got: %v", err)
	}
}

// TestPodsDispatchesCreateToItsOwnSubcommand checks `aq pods create` reaches
// podsCreate rather than the bare list path (which would just print a
// no-args usage/list and ignore "create" as noise).
func TestPodsDispatchesCreateToItsOwnSubcommand(t *testing.T) {
	err := pods([]string{"create"})
	if err == nil || !strings.Contains(err.Error(), "a name is required") {
		t.Fatalf("expected podsCreate's own validation error, got: %v", err)
	}
}
