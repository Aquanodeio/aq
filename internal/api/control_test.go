package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateSnapshotPostsToDeploymentScopedPath is the managed one-shot save
// path (`aq snapshot`): POST /snapshots/:deploymentId/create, team-scoped by the
// x-api-key/x-team-id headers every authenticated Client call carries.
func TestCreateSnapshotPostsToDeploymentScopedPath(t *testing.T) {
	var gotPath, gotKey, gotTeam string
	var gotBody CreateSnapshotRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotTeam = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("x-team-id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"snapshot_id":"snap_abc123","size":104857600,"message":"Snapshot created successfully"}}`)
	}))
	defer srv.Close()

	c := NewAuthed(srv.URL, "tok", "team-1")
	got, err := c.CreateSnapshot(2884, CreateSnapshotRequest{
		BackupID: "aq-2884", WorkspaceDir: "/workspace",
		IncludeProcess: false, IncludeWorkspace: true,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if gotPath != "/snapshots/2884/create" {
		t.Errorf("path = %q, want /snapshots/2884/create", gotPath)
	}
	if gotKey != "tok" || gotTeam != "team-1" {
		t.Errorf("auth headers = %q/%q", gotKey, gotTeam)
	}
	if !gotBody.IncludeWorkspace || gotBody.WorkspaceDir != "/workspace" {
		t.Errorf("body = %+v", gotBody)
	}
	if got.SnapshotID != "snap_abc123" || got.Size != 104857600 {
		t.Errorf("result = %+v", got)
	}
}

// TestCreateSnapshotSurfacesServerError checks a 500 (e.g. an unreachable ogre
// agent) comes back as a Go error rather than a zero-value result.
func TestCreateSnapshotSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"success":false,"error":"Failed to create snapshot"}`)
	}))
	defer srv.Close()
	if _, err := NewAuthed(srv.URL, "tok", "t").CreateSnapshot(1, CreateSnapshotRequest{}); err == nil {
		t.Fatal("want error on 500, got nil")
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
