package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStartSetupPostsOfferSelectionNested checks POST /setups/:id/start sends
// a single already-chosen offer NESTED under "offer" as {resource, provider,
// sshKeyId}: the pod/environment/volume plan's wire contract (section 2,
// REST amendments): image/ports/startup script are NOT here, they come from
// the pod's own config columns, and the orchestrator does no server-side
// matching the way UpRequest/DeployRequest's flattened gpuModel/maxPrice do.
func TestStartSetupPostsOfferSelectionNested(t *testing.T) {
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
		Offer: OfferSelection{
			Resource: ResourceSpec{
				CPU:               8,
				Memory:            "32Gi",
				Storage:           "200Gi",
				GPUUnits:          2,
				GPUModel:          "RTX 4090",
				DesiredInstanceID: "massecompute/abc123",
			},
			Provider: ProviderSpec{Name: "massecompute"},
			SSHKeyID: "key-1",
		},
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
	resource, ok := offer["resource"].(map[string]any)
	if !ok || resource["gpuModel"] != "RTX 4090" {
		t.Errorf("offer.resource = %+v", resource)
	}
	provider, ok := offer["provider"].(map[string]any)
	if !ok || provider["name"] != "massecompute" {
		t.Errorf("offer.provider = %+v", provider)
	}
	if offer["sshKeyId"] != "key-1" {
		t.Errorf("offer.sshKeyId = %v, want key-1", offer["sshKeyId"])
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

// TestStartSetupOmitsUnsetResourceFields checks every optional ResourceSpec
// field is omitempty on the wire: "no opinion" must be an ABSENT key, never
// a zero value the orchestrator could misread as "gpuUnits: 0 GPUs", while
// the required fields (cpu/memory/storage, which have no server-side
// default) are always present even when the caller has nothing better than
// the box defaults.
func TestStartSetupOmitsUnsetResourceFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"x","name":"x","environment":{"id":"e","name":"e","version":null,"kind":"builtin"}}}`)
	}))
	defer srv.Close()

	if _, err := NewAuthed(srv.URL, "tok", "t").StartSetup("x", StartSetupRequest{
		Offer: OfferSelection{
			Resource: ResourceSpec{CPU: 4, Memory: "16Gi", Storage: "100Gi"},
			Provider: ProviderSpec{Name: "runpod"},
			SSHKeyID: "key-1",
		},
	}); err != nil {
		t.Fatalf("StartSetup: %v", err)
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok {
		t.Fatalf("body has no \"offer\" object: %#v", gotBody)
	}
	resource, ok := offer["resource"].(map[string]any)
	if !ok {
		t.Fatalf("offer has no \"resource\" object: %#v", offer)
	}
	for _, key := range []string{"gpuUnits", "gpuModel", "desiredInstanceId", "region", "location_id"} {
		if _, present := resource[key]; present {
			t.Errorf("resource.%s must be ABSENT when unset, got it present: %+v", key, resource)
		}
	}
	for _, key := range []string{"cpu", "memory", "storage"} {
		if _, present := resource[key]; !present {
			t.Errorf("resource.%s is required (no omitempty) and must always be present: %+v", key, resource)
		}
	}
}

// TestStopSetupPostsToStopPath checks POST /setups/:id/stop sends no body
// fields the orchestrator would need to interpret: Stop takes no filter,
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

// TestMoveSetupPostsOfferSelectionNested mirrors
// TestStartSetupPostsOfferSelectionNested for POST /setups/:id/move.
func TestMoveSetupPostsOfferSelectionNested(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"x","name":"trainer","environment":{"id":"e","name":"e","version":null,"kind":"builtin"}}}`)
	}))
	defer srv.Close()

	if _, err := NewAuthed(srv.URL, "tok", "t").MoveSetup("x", MoveSetupRequest{
		Offer: OfferSelection{
			Resource: ResourceSpec{CPU: 4, Memory: "16Gi", Storage: "100Gi"},
			Provider: ProviderSpec{Name: "runpod"},
			SSHKeyID: "key-1",
		},
	}); err != nil {
		t.Fatalf("MoveSetup: %v", err)
	}
	if gotPath != "/setups/x/move" {
		t.Errorf("path = %q, want /setups/x/move", gotPath)
	}
	offer, ok := gotBody["offer"].(map[string]any)
	if !ok {
		t.Fatalf("body has no nested \"offer\" object: %#v", gotBody)
	}
	provider, ok := offer["provider"].(map[string]any)
	if !ok || provider["name"] != "runpod" {
		t.Errorf("body = %#v, want offer.provider.name=runpod", gotBody)
	}
}

// TestSetSetupAutostopPutsToAutostopPath checks PUT /setups/:id/autostop
// (not the old /autopause) with a boolean, non-null Enabled: the CLI itself
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
