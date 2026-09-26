package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestListSetupVersionsQueriesByName checks GET /setups/versions?name=...
// decodes id/version/setup_id, the three fields `aq job point` needs to
// resolve a (setup, version-number) pair to the version's global row id
// without ever guessing.
func TestListSetupVersionsQueriesByName(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[
			{"id":9,"name":"comfyui","version":3,"setup_id":"11111111-1111-1111-1111-111111111111"},
			{"id":10,"name":"comfyui","version":3,"setup_id":"22222222-2222-2222-2222-222222222222"}
		]}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListSetupVersions("comfyui")
	if err != nil {
		t.Fatalf("ListSetupVersions: %v", err)
	}
	if gotPath != "/setups/versions?name=comfyui" {
		t.Errorf("path = %q, want /setups/versions?name=comfyui", gotPath)
	}
	if len(got) != 2 {
		t.Fatalf("got %d versions, want 2", len(got))
	}
	// Two different setups can share a lineage NAME — the same version
	// number under each. A caller must filter on SetupID itself; this test
	// pins that both distinct rows come back rather than being collapsed.
	if got[0].SetupID == got[1].SetupID {
		t.Fatalf("fixture setup ids collided: %+v / %+v", got[0], got[1])
	}
}

// TestListAllSetupVersionsQueriesWithNoNameFilter checks GET /setups/versions
// with no `name` query param, the path `aq pods`/`aq job create` use to
// recover a setup's latest/named version, since GET /setups carries no such
// field nested on the row itself (see the Setup doc comment in setups.go).
func TestListAllSetupVersionsQueriesWithNoNameFilter(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[
			{"id":9,"name":"comfyui","version":3,"setup_id":"11111111-1111-1111-1111-111111111111"},
			{"id":8,"name":"comfyui","version":2,"setup_id":"11111111-1111-1111-1111-111111111111"}
		]}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListAllSetupVersions()
	if err != nil {
		t.Fatalf("ListAllSetupVersions: %v", err)
	}
	if gotURL != "/setups/versions" {
		t.Errorf("url = %q, want /setups/versions (no name filter)", gotURL)
	}
	if len(got) != 2 {
		t.Fatalf("got %d versions, want 2", len(got))
	}
}

// TestListSetupsDecodesOwnedSetups checks `aq pods` decodes the fields it
// renders, including deriving Running from attachedDeploymentId and reading
// stopping: there is no boolean "running" field on the wire. The fixture
// matches the confirmed PodDTO shape (w3-backend, pod-serializer.ts,
// 2026-09-26): the pre-pod/environment/volume serializeSetup's
// `leaseDeploymentId`/`sizeBytes`/`mountPath`/`lastSyncAt` are gone from the
// wire entirely, not renamed, so this fixture omits them rather than
// asserting a fictional field decodes to nil. There is likewise no
// "latest_version"/"latestVersion" field at all — GET /setups never sends
// one (see ListAllSetupVersions's doc comment).
func TestListSetupsDecodesOwnedSetups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setups" {
			t.Errorf("path = %q, want /setups", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[
			{"id":"11111111-1111-1111-1111-111111111111","name":"comfyui","status":"ACTIVE","attachedDeploymentId":42,"stopping":false},
			{"id":"22222222-2222-2222-2222-222222222222","name":"jupyter","status":"STOPPING","attachedDeploymentId":43,"stopping":true},
			{"id":"33333333-3333-3333-3333-333333333333","name":"idle","status":"STOPPED","attachedDeploymentId":null,"stopping":false}
		]}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListSetups()
	if err != nil {
		t.Fatalf("ListSetups: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d setups, want 3", len(got))
	}
	if !got[0].Running() || got[0].Stopping {
		t.Errorf("got[0]: Running()=%v Stopping=%v, want Running=true Stopping=false (attachedDeploymentId=42)", got[0].Running(), got[0].Stopping)
	}
	if !got[1].Running() || !got[1].Stopping {
		t.Errorf("got[1]: Running()=%v Stopping=%v, want both true (attachedDeploymentId=43, stopping=true: a Stop in flight is still attached)", got[1].Running(), got[1].Stopping)
	}
	if got[2].Running() || got[2].Stopping {
		t.Errorf("got[2]: Running()=%v Stopping=%v, want both false (attachedDeploymentId=null)", got[2].Running(), got[2].Stopping)
	}
}

// TestListSetupsDecodesRealEnvironmentAndVolumeFixture pins the environment/
// volume shape against a trimmed copy of a REAL captured GET /setups row
// (prod, 2026-09-27, team's own "MI300X box 11" test pod; an id is not a
// customer's name or email, so it is kept rather than replaced with a
// fictional one). Two facts this must not get wrong: SizeBytes:0 is a real
// MEASURED zero and must decode to a non-nil pointer (never collapse to the
// same nil an unmeasured volume uses), and a `null` environment (see
// TestListSetupsDecodesNullEnvironment below) must decode to a nil pointer,
// not a zero-valued-but-non-nil struct that renders as if it were a real,
// if empty, answer.
func TestListSetupsDecodesRealEnvironmentAndVolumeFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{
			"id":"b8591f0f-038e-4d48-b9a2-26a3b7caeadd","name":"MI300X box 11","status":"ready",
			"autostopEnabled":null,"attachedDeploymentId":null,"stopping":false,"lastError":null,
			"environment":{"id":"e4cf117d-0144-4d75-9b25-72c10aa6ad62","name":"Ubuntu 24.04 + CUDA","version":null,"kind":"builtin"},
			"volume":{"id":"47a69ae8-f1d5-418f-8687-928bb4c464ff","name":"MI300X box 11","sizeBytes":8270434,"headSavedAt":"2026-09-26T20:06:46.999Z","saveState":"saved","lastSaveError":null},
			"restore":null,"createdAt":"2026-09-26T19:51:35.418Z","updatedAt":"2026-09-26T20:35:47.857Z"
		},{
			"id":"c5c94a9c-cea9-4530-80b4-09072f2f4cc3","name":"My setup 32","status":"ready",
			"autostopEnabled":null,"attachedDeploymentId":null,"stopping":false,"lastError":null,
			"environment":{"id":"e4cf117d-0144-4d75-9b25-72c10aa6ad62","name":"Ubuntu 24.04 + CUDA","version":null,"kind":"builtin"},
			"volume":{"id":"c5c94a9c-cea9-4530-80b4-09072f2f4cc3","name":"My setup 32","sizeBytes":0,"headSavedAt":null,"saveState":"unknown","lastSaveError":null},
			"restore":null,"createdAt":"2026-09-26T19:00:00.000Z","updatedAt":"2026-09-26T19:00:00.000Z"
		}]}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListSetups()
	if err != nil {
		t.Fatalf("ListSetups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d setups, want 2", len(got))
	}

	measured := got[0]
	if measured.Environment == nil || measured.Environment.Kind != "builtin" || measured.Environment.Version != nil {
		t.Errorf("measured.Environment = %+v, want a non-nil builtin environment with Version nil", measured.Environment)
	}
	if measured.Volume == nil || measured.Volume.SizeBytes == nil || *measured.Volume.SizeBytes != 8270434 {
		t.Errorf("measured.Volume = %+v, want SizeBytes 8270434", measured.Volume)
	}
	if measured.Volume.HeadSavedAt == nil || *measured.Volume.HeadSavedAt != "2026-09-26T20:06:46.999Z" {
		t.Errorf("measured.Volume.HeadSavedAt = %v, want the real timestamp", measured.Volume.HeadSavedAt)
	}

	zero := got[1]
	// sizeBytes: 0 is a REAL measurement (storage metering ran and found ~0
	// bytes), distinct from sizeBytes: null (never measured). Both must not
	// collapse to a nil pointer.
	if zero.Volume == nil || zero.Volume.SizeBytes == nil || *zero.Volume.SizeBytes != 0 {
		t.Errorf("zero.Volume = %+v, want a non-nil SizeBytes pointing at 0 (a real measured zero)", zero.Volume)
	}
	if zero.Volume.HeadSavedAt != nil {
		t.Errorf("zero.Volume.HeadSavedAt = %v, want nil (never saved)", zero.Volume.HeadSavedAt)
	}
}

// TestListSetupsDecodesNullEnvironment covers the wire shape no live row in
// the 224-pod capture above happened to have (pod-serializer.ts's ternary
// returns null when environmentVersionId doesn't resolve to a live version,
// sourced from reading the serializer, not observed live): Setup.Environment
// must decode to nil, not a zero-valued struct whose empty Name then renders
// identically to a real (if plain) one. This is the actual mechanism behind
// the reported "environment version -" mismatch against the console.
func TestListSetupsDecodesNullEnvironment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{"id":"11111111-1111-1111-1111-111111111111","name":"orphaned",`+
			`"status":"ready","attachedDeploymentId":null,"stopping":false,"environment":null,"volume":null}]}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListSetups()
	if err != nil {
		t.Fatalf("ListSetups: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d setups, want 1", len(got))
	}
	if got[0].Environment != nil {
		t.Errorf("Environment = %+v, want nil for a wire `null`", got[0].Environment)
	}
	if got[0].Volume != nil {
		t.Errorf("Volume = %+v, want nil for a bare pod", got[0].Volume)
	}
}

// TestGetSetupDecodesOnePod checks GET /setups/:id, the lookup `aq status`
// uses to recover a pod's real save state from a deployment's setup_id, hits
// the right path and decodes the same DTO shape ListSetups does.
func TestGetSetupDecodesOnePod(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"b8591f0f-038e-4d48-b9a2-26a3b7caeadd","name":"MI300X box 11",`+
			`"status":"ready","attachedDeploymentId":null,"stopping":false,`+
			`"volume":{"id":"47a69ae8-f1d5-418f-8687-928bb4c464ff","name":"MI300X box 11","sizeBytes":8270434,`+
			`"headSavedAt":"2026-09-26T20:06:46.999Z","saveState":"saved","lastSaveError":null}}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").GetSetup("b8591f0f-038e-4d48-b9a2-26a3b7caeadd")
	if err != nil {
		t.Fatalf("GetSetup: %v", err)
	}
	if gotPath != "/setups/b8591f0f-038e-4d48-b9a2-26a3b7caeadd" {
		t.Errorf("path = %q, want /setups/<id>", gotPath)
	}
	if got.Volume == nil || got.Volume.SaveState != "saved" {
		t.Errorf("Volume = %+v, want SaveState \"saved\"", got.Volume)
	}
}
