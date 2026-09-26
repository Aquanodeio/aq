package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetVolumeDecodesARealShapedRow checks GET /volumes/:id against the
// confirmed VolumeDTO shape (orchestrator/src/services/volumes/
// volume.service.ts, w3-backend 2026-09-26): attachedPodId/attachedPodName/
// running (not the pre-model attachedToPodId), headSavedAt/saveState/
// lastSaveError (not lastSyncedAt/lastSyncError), a numeric sizeBytes, and
// createdAt. Also decodes the nested points array, and that a point with no
// label decodes to a nil pointer rather than an empty string, the two must
// stay distinguishable since a migrated legacy version keeps its label
// (D10) and a plain Stop point never had one.
func TestGetVolumeDecodesARealShapedRow(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{
			"id":"vol-1","name":"research-data","sizeBytes":51539607552,
			"mountPath":"/workspace","attachedPodId":"pod-1","attachedPodName":"trainer",
			"running":true,"headSavedAt":"2026-09-26T00:00:00Z","saveState":"saved",
			"lastSaveError":null,"createdAt":"2026-09-01T00:00:00Z",
			"points":[
				{"id":"pt-1","createdAt":"2026-09-25T00:00:00Z","provenance":"stop","label":null},
				{"id":"pt-0","createdAt":"2026-09-01T00:00:00Z","provenance":"idle_stop","label":"before-migration"}
			]
		}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").GetVolume("vol-1")
	if err != nil {
		t.Fatalf("GetVolume: %v", err)
	}
	if gotPath != "/volumes/vol-1" {
		t.Errorf("path = %q, want /volumes/vol-1", gotPath)
	}
	if len(got.Points) != 2 {
		t.Fatalf("got %d points, want 2", len(got.Points))
	}
	if got.Points[0].Label != nil {
		t.Errorf("Points[0].Label = %v, want nil (never labeled)", got.Points[0].Label)
	}
	if got.Points[1].Label == nil || *got.Points[1].Label != "before-migration" {
		t.Errorf("Points[1].Label = %v, want \"before-migration\"", got.Points[1].Label)
	}
	if got.AttachedPodID == nil || *got.AttachedPodID != "pod-1" {
		t.Errorf("AttachedPodID = %v, want pod-1", got.AttachedPodID)
	}
	if got.AttachedPodName == nil || *got.AttachedPodName != "trainer" {
		t.Errorf("AttachedPodName = %v, want trainer", got.AttachedPodName)
	}
	if !got.Running {
		t.Error("Running = false, want true")
	}
	if got.SizeBytes == nil || *got.SizeBytes != 51539607552 {
		t.Errorf("SizeBytes = %v, want 51539607552", got.SizeBytes)
	}
	if got.HeadSavedAt == nil || *got.HeadSavedAt != "2026-09-26T00:00:00Z" {
		t.Errorf("HeadSavedAt = %v, want 2026-09-26T00:00:00Z", got.HeadSavedAt)
	}
	if got.SaveState != "saved" {
		t.Errorf("SaveState = %q, want saved", got.SaveState)
	}
	if got.CreatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q, want 2026-09-01T00:00:00Z", got.CreatedAt)
	}
}

// TestListVolumesDecodesUnattachedUnmeasuredVolume checks a volume with no
// attached pod and no metering pass yet decodes AttachedPodID/SizeBytes/
// HeadSavedAt as nil, never a zero value or empty string that reads as a
// real measurement or an attachment to pod "".
func TestListVolumesDecodesUnattachedUnmeasuredVolume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/volumes" {
			t.Errorf("path = %q, want /volumes", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{
			"id":"vol-2","name":"scratch","sizeBytes":null,"mountPath":"/workspace",
			"attachedPodId":null,"attachedPodName":null,"running":false,
			"headSavedAt":null,"saveState":"unknown","lastSaveError":null,
			"createdAt":"2026-09-20T00:00:00Z","points":[]
		}]}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").ListVolumes()
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d volumes, want 1", len(got))
	}
	v := got[0]
	if v.AttachedPodID != nil {
		t.Errorf("AttachedPodID = %v, want nil", v.AttachedPodID)
	}
	if v.SizeBytes != nil {
		t.Errorf("SizeBytes = %v, want nil (never measured), not zero", v.SizeBytes)
	}
	if v.HeadSavedAt != nil {
		t.Errorf("HeadSavedAt = %v, want nil (never saved)", v.HeadSavedAt)
	}
	if v.Running {
		t.Error("Running = true, want false for an unattached volume")
	}
	if v.SaveState != "unknown" {
		t.Errorf("SaveState = %q, want unknown", v.SaveState)
	}
}

// TestDuplicateVolumePostsName checks POST /volumes/:id/duplicate sends
// {name}.
func TestDuplicateVolumePostsName(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"vol-3","name":"research-data-copy"}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").DuplicateVolume("vol-1", "research-data-copy")
	if err != nil {
		t.Fatalf("DuplicateVolume: %v", err)
	}
	if gotPath != "/volumes/vol-1/duplicate" {
		t.Errorf("path = %q, want /volumes/vol-1/duplicate", gotPath)
	}
	if gotBody["name"] != "research-data-copy" {
		t.Errorf("body = %#v, want name=research-data-copy", gotBody)
	}
	if got.Name != "research-data-copy" {
		t.Errorf("result name = %q", got.Name)
	}
}

// TestRestoreVolumePointPostsPointID checks POST /volumes/:id/restore sends
// {pointId}, restoring is always an exact point, never "newest by time".
func TestRestoreVolumePointPostsPointID(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"vol-1","name":"research-data"}}`)
	}))
	defer srv.Close()

	if _, err := NewAuthed(srv.URL, "tok", "t").RestoreVolumePoint("vol-1", "pt-0"); err != nil {
		t.Fatalf("RestoreVolumePoint: %v", err)
	}
	if gotPath != "/volumes/vol-1/restore" {
		t.Errorf("path = %q, want /volumes/vol-1/restore", gotPath)
	}
	if gotBody["pointId"] != "pt-0" {
		t.Errorf("body = %#v, want pointId=pt-0", gotBody)
	}
}

// TestDeleteVolumeSendsDelete checks DELETE /volumes/:id.
func TestDeleteVolumeSendsDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":null}`)
	}))
	defer srv.Close()

	if err := NewAuthed(srv.URL, "tok", "t").DeleteVolume("vol-1"); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/volumes/vol-1" {
		t.Errorf("%s %s, want DELETE /volumes/vol-1", gotMethod, gotPath)
	}
}

// TestDeleteVolumeSurfacesConflictWhileAttached checks a 409 from the
// orchestrator (attached-volume refusal) surfaces as a real error, not a
// silently swallowed success.
func TestDeleteVolumeSurfacesConflictWhileAttached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "volume is attached to a running pod"})
	}))
	defer srv.Close()

	err := NewAuthed(srv.URL, "tok", "t").DeleteVolume("vol-1")
	if err == nil {
		t.Fatal("want an error on 409, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != http.StatusConflict {
		t.Errorf("expected an APIError with status 409, got: %v", err)
	}
}
