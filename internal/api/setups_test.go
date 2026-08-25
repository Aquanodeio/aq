package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateSetupSnapshotPostsNameAndWorkspaceDir checks `aq snapshot`'s API
// call: POST /setups/:id/snapshot with the lineage name and the captured
// directory under its wire name workspace_dir (not a renamed "path" field —
// --path is a CLI-surface rename only), and that the returned version row
// decodes for the "✓ Saved <name> v<version>" print.
func TestCreateSetupSnapshotPostsNameAndWorkspaceDir(t *testing.T) {
	var gotPath string
	var gotBody CreateSetupSnapshotRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":9,"name":"comfyui","version":3,"setup_id":"11111111-1111-1111-1111-111111111111","size_bytes":1073741824}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").CreateSetupSnapshot("11111111-1111-1111-1111-111111111111", CreateSetupSnapshotRequest{
		Name: "comfyui", WorkspaceDir: "/workspace",
	})
	if err != nil {
		t.Fatalf("CreateSetupSnapshot: %v", err)
	}
	if gotPath != "/setups/11111111-1111-1111-1111-111111111111/snapshot" {
		t.Errorf("path = %q, want /setups/<uuid>/snapshot", gotPath)
	}
	if gotBody.Name != "comfyui" || gotBody.WorkspaceDir != "/workspace" {
		t.Errorf("body = %+v", gotBody)
	}
	if got.Name != "comfyui" || got.Version != 3 {
		t.Errorf("result = %+v, want name=comfyui version=3", got)
	}
}

// TestListSetupVersionsQueriesByName checks GET /setups/versions?name=...
// decodes id/version/setup_id — the three fields `aq share` needs to resolve
// a (setup, version-number) pair to the version's global row id without ever
// guessing.
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
// with no `name` query param — the path `aq setups`/`aq share` use to recover
// a setup's latest/named version, since GET /setups carries no such field
// nested on the row itself (see the Setup doc comment in setups.go).
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

// TestShareSetupVersionPostsToVersionScopedPath checks `aq share` hits the
// version-scoped route by the version's global ROW id, not a setup-scoped
// one and not the per-lineage version number — a share link addresses one
// immutable version, never a moving lineage head. It also pins the real
// server contract: createVersionShare (snapshot-version.service.ts) returns
// a bare {token,name,expires_at} — never a url — and ShareSetupVersion must
// build the public /launch/<token> link itself from Token.
func TestShareSetupVersionPostsToVersionScopedPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"token":"abc123","name":null,"expires_at":null}}`)
	}))
	defer srv.Close()

	t.Setenv("AQ_CONSOLE_URL", "https://console.aquanode.io")
	got, err := NewAuthed(srv.URL, "tok", "t").ShareSetupVersion(9)
	if err != nil {
		t.Fatalf("ShareSetupVersion: %v", err)
	}
	if gotPath != "/setups/versions/9/share" {
		t.Errorf("path = %q, want /setups/versions/9/share", gotPath)
	}
	if got.Token != "abc123" {
		t.Errorf("Token = %q, want abc123", got.Token)
	}
	if got.URL != "https://console.aquanode.io/launch/abc123" {
		t.Errorf("URL = %q", got.URL)
	}
}

// TestSetSetupAutosavePutsEnabledFlag checks `aq autosave` sends a plain
// {enabled: bool} body via PUT, and decodes the returned Setup row (not a
// bespoke {enabled} result shape). The fixture uses `autosaveEnabled` —
// serializeSetup (setups.controller.ts) hand-writes camelCase for every
// `Setup` field; see the package doc comment in setups.go for why.
func TestSetSetupAutosavePutsEnabledFlag(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody SetupAutosaveRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"11111111-1111-1111-1111-111111111111","name":"comfyui","autosaveEnabled":true}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").SetSetupAutosave("11111111-1111-1111-1111-111111111111", true)
	if err != nil {
		t.Fatalf("SetSetupAutosave: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/setups/11111111-1111-1111-1111-111111111111/autosave" {
		t.Errorf("method/path = %s %s, want PUT /setups/<uuid>/autosave", gotMethod, gotPath)
	}
	if !gotBody.Enabled {
		t.Errorf("body = %+v, want enabled=true", gotBody)
	}
	if !got.AutosaveEnabled {
		t.Errorf("result = %+v, want autosave_enabled=true", got)
	}
}

// TestListSetupsDecodesOwnedSetups checks `aq setups` decodes the fields it
// renders, including deriving Running from leaseDeploymentId — there is no
// boolean "running" field on the wire. The fixture is camelCase throughout
// (serializeSetup's real wire shape, see the package doc comment in
// setups.go) with `sizeBytes` as the decimal STRING serializeSetup actually
// sends (BigInt doesn't survive JSON.stringify) — a plain JSON number here
// would pass against a wrong Go type just as easily as a string, so this
// pins the real shape, not just a decodable one. There is no
// "latest_version"/"latestVersion" field at all — GET /setups never sends
// one (see ListAllSetupVersions's doc comment) — so this fixture omits it
// rather than asserting a fictional field decodes to nil.
func TestListSetupsDecodesOwnedSetups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/setups" {
			t.Errorf("path = %q, want /setups", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[
			{"id":"11111111-1111-1111-1111-111111111111","name":"comfyui","status":"ACTIVE","sizeBytes":"1073741824","leaseDeploymentId":42},
			{"id":"22222222-2222-2222-2222-222222222222","name":"jupyter","status":"CLOSED","sizeBytes":null,"leaseDeploymentId":null}
		]}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListSetups()
	if err != nil {
		t.Fatalf("ListSetups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d setups, want 2", len(got))
	}
	if !got[0].Running() {
		t.Errorf("got[0].Running() = false, want true (leaseDeploymentId=42)")
	}
	if got[0].SizeBytes != 1073741824 {
		t.Errorf("got[0].SizeBytes = %d", got[0].SizeBytes)
	}
	if got[1].Running() {
		t.Errorf("got[1].Running() = true, want false (leaseDeploymentId=null)")
	}
	if got[1].SizeBytes != 0 {
		t.Errorf("got[1].SizeBytes = %d, want 0 (sizeBytes=null)", got[1].SizeBytes)
	}
}
