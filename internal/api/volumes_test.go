package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetVolumeDecodesARealShapedRow checks GET /volumes/:id against the
// confirmed VolumeDetailDTO shape (orchestrator/src/services/volumes/
// volume.service.ts, w3-backend PR #811, 2026-09-28): inUseBy/copying (not
// the pre-model attachedPodId/attachedPodName/running), usedBy (any number
// of referencing pods, running or stopped), headSavedAt/saveState/
// lastSaveError, a numeric sizeBytes, and createdAt. Also decodes the
// nested points array, and that a point with no label decodes to a nil
// pointer rather than an empty string, the two must stay distinguishable
// since a migrated legacy version keeps its label (D10) and a plain Stop
// point never had one.
func TestGetVolumeDecodesARealShapedRow(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{
			"id":"vol-1","name":"research-data","sizeBytes":51539607552,
			"mountPath":"/workspace","inUseBy":{"podId":"pod-1","podName":"trainer"},
			"copying":false,"headSavedAt":"2026-09-26T00:00:00Z","saveState":"saved",
			"lastSaveError":null,"createdAt":"2026-09-01T00:00:00Z",
			"usedBy":[
				{"podId":"pod-1","podName":"trainer","state":"running"},
				{"podId":"pod-2","podName":"trainer-b","state":"stopped"}
			],
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
	if got.InUseBy == nil || got.InUseBy.PodID == nil || *got.InUseBy.PodID != "pod-1" {
		t.Errorf("InUseBy.PodID = %v, want pod-1", got.InUseBy)
	}
	if got.InUseBy == nil || got.InUseBy.PodName == nil || *got.InUseBy.PodName != "trainer" {
		t.Errorf("InUseBy.PodName = %v, want trainer", got.InUseBy)
	}
	if got.Copying {
		t.Error("Copying = true, want false")
	}
	if len(got.UsedBy) != 2 {
		t.Fatalf("got %d usedBy, want 2", len(got.UsedBy))
	}
	if got.UsedBy[0] != (VolumeUser{PodID: "pod-1", PodName: "trainer", State: "running"}) {
		t.Errorf("UsedBy[0] = %+v, want the running holder", got.UsedBy[0])
	}
	if got.UsedBy[1] != (VolumeUser{PodID: "pod-2", PodName: "trainer-b", State: "stopped"}) {
		t.Errorf("UsedBy[1] = %+v, want the stopped referencing pod", got.UsedBy[1])
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

// TestListVolumesDecodesUnusedUnmeasuredVolume checks a volume no pod's live
// box has and no metering pass has ever measured decodes InUseBy/SizeBytes/
// HeadSavedAt as nil, never a zero value or empty string that reads as a
// real measurement or a hidden holder. Also pins the "never_saved" wire
// value (#811 renamed it from "unknown") decodes verbatim, never collapsed
// into "saved".
func TestListVolumesDecodesUnusedUnmeasuredVolume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/volumes" {
			t.Errorf("path = %q, want /volumes", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{
			"id":"vol-2","name":"scratch","sizeBytes":null,"mountPath":"/workspace",
			"inUseBy":null,"copying":false,
			"headSavedAt":null,"saveState":"never_saved","lastSaveError":null,
			"createdAt":"2026-09-20T00:00:00Z"
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
	if v.InUseBy != nil {
		t.Errorf("InUseBy = %v, want nil", v.InUseBy)
	}
	if v.SizeBytes != nil {
		t.Errorf("SizeBytes = %v, want nil (never measured), not zero", v.SizeBytes)
	}
	if v.HeadSavedAt != nil {
		t.Errorf("HeadSavedAt = %v, want nil (never saved)", v.HeadSavedAt)
	}
	if v.Copying {
		t.Error("Copying = true, want false")
	}
	if v.SaveState != "never_saved" {
		t.Errorf("SaveState = %q, want never_saved", v.SaveState)
	}
	if len(v.UsedBy) != 0 || len(v.Points) != 0 {
		t.Errorf("UsedBy/Points = %v/%v, want both empty on a list row", v.UsedBy, v.Points)
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

// TestDeleteVolumeReturnsDetachedPods checks DELETE /volumes/:id decodes the
// id plus every stopped pod the delete detached.
func TestDeleteVolumeReturnsDetachedPods(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"id":"vol-1","detachedPods":[{"podId":"pod-2","podName":"trainer-b"}]}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").DeleteVolume("vol-1")
	if err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/volumes/vol-1" {
		t.Errorf("%s %s, want DELETE /volumes/vol-1", gotMethod, gotPath)
	}
	if got.ID != "vol-1" {
		t.Errorf("ID = %q, want vol-1", got.ID)
	}
	if len(got.DetachedPods) != 1 || got.DetachedPods[0] != (DetachedPod{PodID: "pod-2", PodName: "trainer-b"}) {
		t.Errorf("DetachedPods = %+v, want one row for pod-2/trainer-b", got.DetachedPods)
	}
}

// TestDeleteVolumeSurfacesInUseConflict checks a 409 from the orchestrator
// (a pod's live box has the volume) surfaces as a *VolumeInUseError naming
// the holder, not a silently swallowed success and not a plain *APIError
// that loses the holder.
func TestDeleteVolumeSurfacesInUseConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   "Volume research-data is in use by pod trainer. Stop trainer first.",
			"data":    map[string]any{"code": "PLACEMENT_VOLUME_IN_USE", "inUseBy": map[string]any{"podId": "pod-1", "podName": "trainer"}},
		})
	}))
	defer srv.Close()

	_, err := NewAuthed(srv.URL, "tok", "t").DeleteVolume("vol-1")
	if err == nil {
		t.Fatal("want an error on 409, got nil")
	}
	inUseErr, ok := err.(*VolumeInUseError)
	if !ok {
		t.Fatalf("expected a *VolumeInUseError, got: %T (%v)", err, err)
	}
	if inUseErr.InUseBy == nil || inUseErr.InUseBy.PodName == nil || *inUseErr.InUseBy.PodName != "trainer" {
		t.Errorf("InUseBy = %v, want podName trainer", inUseErr.InUseBy)
	}
}
