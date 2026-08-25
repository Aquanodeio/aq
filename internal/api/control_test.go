package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetDeploymentReturnsProjectID checks GetDeployment (the raw
// GET /deployments/:id row) decodes project_id — the field `aq park` needs
// to hit the project-scoped pause route, which the transformed /status and
// list endpoints don't carry.
func TestGetDeploymentReturnsProjectID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/deployments/2884" {
			t.Errorf("path = %q, want /deployments/2884", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":2884,"name":"comfyui","status":"ACTIVE","project_id":"11111111-1111-1111-1111-111111111111"}}`)
	}))
	defer srv.Close()

	dep, err := NewAuthed(srv.URL, "tok", "t").GetDeployment(2884)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if dep.ProjectID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("ProjectID = %q, want the project uuid", dep.ProjectID)
	}
}

// TestParkDeploymentPostsToProjectScopedPath checks ParkDeployment (`aq
// park`) hits the existing /deployments/project/:projectId/pause route with
// deploymentId in the body, not a hypothetical deployment-scoped pause
// endpoint. The route itself is still named "pause" server-side — only the
// CLI verb was renamed to "park", see park.go's doc comment for why.
func TestParkDeploymentPostsToProjectScopedPath(t *testing.T) {
	var gotPath string
	var gotBody map[string]int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":null}`)
	}))
	defer srv.Close()

	if err := NewAuthed(srv.URL, "tok", "t").ParkDeployment("proj-1", 2884); err != nil {
		t.Fatalf("ParkDeployment: %v", err)
	}
	if gotPath != "/deployments/project/proj-1/pause" {
		t.Errorf("path = %q, want /deployments/project/proj-1/pause", gotPath)
	}
	if gotBody["deploymentId"] != 2884 {
		t.Errorf("body = %+v, want deploymentId 2884", gotBody)
	}
}

// TestSnapshotHistoryAssociatesByBackupsDeploymentID checks the match key used
// to find "the last snapshot for deployment N": the account-scoped history
// endpoint nests a snapshot's owning deployment under `backups.deployment_id`,
// not the top-level `backup_id` (that field is the internal backup ROW id, not
// a deployment id).
func TestSnapshotHistoryAssociatesByBackupsDeploymentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/snapshots/history" {
			t.Errorf("path = %q, want /snapshots/history", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[
			{"id":42,"backup_id":7,"path":"/workspace","status":"completed","size":100,"type":"manual","created_at":"2026-08-07T11:30:00Z","backups":{"deployment_id":2884,"path":"/workspace"}},
			{"id":9,"backup_id":3,"path":"/workspace","status":"completed","size":50,"type":"external","created_at":"2026-08-06T09:00:00Z","backups":null}
		]}`)
	}))
	defer srv.Close()

	items, err := NewAuthed(srv.URL, "tok", "t").SnapshotHistory()
	if err != nil {
		t.Fatalf("SnapshotHistory: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Backups == nil || items[0].Backups.DeploymentID != 2884 {
		t.Errorf("items[0].Backups = %+v, want deployment_id 2884", items[0].Backups)
	}
	if items[1].Backups != nil {
		t.Errorf("items[1].Backups = %+v, want nil (external snapshot)", items[1].Backups)
	}
}
