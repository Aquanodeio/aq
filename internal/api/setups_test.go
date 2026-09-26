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
// stopping — there is no boolean "running" field on the wire. The fixture
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
