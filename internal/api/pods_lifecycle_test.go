package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStartSetupPostsOfferFilterNested checks POST /setups/:id/start sends
// the GPU filter fields NESTED under "offer" (not flattened like
// UpRequest/DeployRequest) — that nesting is the one thing the
// pod/environment/volume plan's wire contract states explicitly for this
// route, and a caller that flattens them silently sends an offer the
// orchestrator never sees.
func TestStartSetupPostsOfferFilterNested(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"11111111-1111-1111-1111-111111111111","name":"trainer","mountPath":"/workspace","environment":{"id":"e1","name":"pytorch-dev","version":3,"kind":"builtin"},"volume":null,"restore":null,"autostopEnabled":null,"sizeBytes":null,"lastSyncAt":"","leaseDeploymentId":null,"createdAt":"","updatedAt":""}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").StartSetup("11111111-1111-1111-1111-111111111111", StartSetupRequest{
		Offer: OfferFilter{GPUModel: "RTX 4090", MaxPrice: 1.5, Provider: "massecompute", GPUCount: 2},
	})
	if err != nil {
		t.Fatalf("StartSetup: %v", err)
	}
	if gotPath != "/setups/11111111-1111-1111-1111-111111111111/start" {
		t.Errorf("path = %q, want /setups/<uuid>/start", gotPath)
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok {
		t.Fatalf("body has no nested \"offer\" object: %#v", gotBody)
	}
	if offer["gpuModel"] != "RTX 4090" || offer["provider"] != "massecompute" {
		t.Errorf("offer body = %+v", offer)
	}
	if got.Name != "trainer" {
		t.Errorf("result name = %q, want trainer", got.Name)
	}
	if got.Environment.Name != "pytorch-dev" || got.Environment.Version == nil || *got.Environment.Version != 3 {
		t.Errorf("environment summary decoded wrong: %+v", got.Environment)
	}
	if got.Volume != nil {
		t.Errorf("volume should decode nil for a pod with none, got %+v", got.Volume)
	}
}

// TestStartSetupOmitsUnsetOfferFields checks every OfferFilter field is
// omitempty on the wire — "no opinion" must be an ABSENT key, never a zero
// value the orchestrator could misread as "gpuCount: 0 GPUs" or
// "maxPrice: $0/hr".
func TestStartSetupOmitsUnsetOfferFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"x","name":"x","environment":{"id":"e","name":"e","version":null,"kind":"builtin"}}}`)
	}))
	defer srv.Close()

	if _, err := NewAuthed(srv.URL, "tok", "t").StartSetup("x", StartSetupRequest{}); err != nil {
		t.Fatalf("StartSetup: %v", err)
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok {
		t.Fatalf("body has no \"offer\" object: %#v", gotBody)
	}
	for _, key := range []string{"gpuModel", "maxPrice", "provider", "gpuCount"} {
		if _, present := offer[key]; present {
			t.Errorf("offer.%s must be ABSENT when unset, got it present: %+v", key, offer)
		}
	}
}

// TestStopSetupPostsToStopPath checks POST /setups/:id/stop sends no body
// fields the orchestrator would need to interpret — Stop takes no filter,
// unlike Start/Move.
func TestStopSetupPostsToStopPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"x","name":"trainer","environment":{"id":"e","name":"e","version":null,"kind":"builtin"}}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").StopSetup("x")
	if err != nil {
		t.Fatalf("StopSetup: %v", err)
	}
	if gotPath != "/setups/x/stop" {
		t.Errorf("path = %q, want /setups/x/stop", gotPath)
	}
	if got.Name != "trainer" {
		t.Errorf("result name = %q, want trainer", got.Name)
	}
}

// TestMoveSetupPostsOfferFilterNested mirrors TestStartSetupPostsOfferFilterNested
// for POST /setups/:id/move.
func TestMoveSetupPostsOfferFilterNested(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"x","name":"trainer","environment":{"id":"e","name":"e","version":null,"kind":"builtin"}}}`)
	}))
	defer srv.Close()

	if _, err := NewAuthed(srv.URL, "tok", "t").MoveSetup("x", MoveSetupRequest{Offer: OfferFilter{Provider: "runpod"}}); err != nil {
		t.Fatalf("MoveSetup: %v", err)
	}
	if gotPath != "/setups/x/move" {
		t.Errorf("path = %q, want /setups/x/move", gotPath)
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok || offer["provider"] != "runpod" {
		t.Errorf("body = %#v, want offer.provider=runpod", gotBody)
	}
}

// TestSetSetupAutostopPutsToAutostopPath checks PUT /setups/:id/autostop
// (not the old /autopause) with a boolean, non-null Enabled — the CLI itself
// never sends null, but the field stays a pointer end to end so a future
// verb can.
func TestSetSetupAutostopPutsToAutostopPath(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"x","name":"trainer","autostopEnabled":true,"environment":{"id":"e","name":"e","version":null,"kind":"builtin"}}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").SetSetupAutostop("x", true)
	if err != nil {
		t.Fatalf("SetSetupAutostop: %v", err)
	}
	if gotPath != "/setups/x/autostop" || gotMethod != http.MethodPut {
		t.Errorf("%s %s, want PUT /setups/x/autostop", gotMethod, gotPath)
	}
	if gotBody["enabled"] != true {
		t.Errorf("body = %#v, want enabled:true", gotBody)
	}
	if got.AutostopEnabled == nil || !*got.AutostopEnabled {
		t.Errorf("AutostopEnabled = %v, want true", got.AutostopEnabled)
	}
}
