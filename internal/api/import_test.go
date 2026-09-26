package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCompleteImportSendsExactlyTheDefinedFields checks POST
// /volumes/import/complete's body never carries path/size: completeImportSchema
// (orchestrator/src/schemas/volumes.schemas.ts:103-108) defines only
// volume_id, import_token, ogre_snapshot_id, and observation. aq used to
// also send path/size, which the schema silently strips (zod's default
// "strip unknown keys" mode, no error), exactly the kind of dead,
// never-validated field rule 4 says to delete rather than keep sending.
func TestCompleteImportSendsExactlyTheDefinedFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"volume_id":"vol-1","warnings":[]}}`)
	}))
	defer srv.Close()

	if _, err := NewAuthed(srv.URL, "tok", "t").CompleteImport(ImportCompleteRequest{
		VolumeID:       "vol-1",
		ImportToken:    "tok-1",
		OgreSnapshotID: "snap-1",
		Observation:    ImportObservation{Schema: ImportObservationSchema},
	}); err != nil {
		t.Fatalf("CompleteImport: %v", err)
	}

	for _, key := range []string{"path", "size"} {
		if _, present := gotBody[key]; present {
			t.Errorf("body must not carry %q, the orchestrator's schema never defined it; got: %#v", key, gotBody)
		}
	}
	for _, key := range []string{"volume_id", "import_token", "ogre_snapshot_id", "observation"} {
		if _, present := gotBody[key]; !present {
			t.Errorf("body is missing required key %q; got: %#v", key, gotBody)
		}
	}
}

// TestCompleteImportOmitsUnreportedHostAndGPUFields checks a partially
// (or entirely) unreported Host/GPU marshals with exactly the reported
// fields present and every unreported one ABSENT, never a zero-valued ""
// or 0 standing in for "ogre never told us this" (importObservationSchema,
// orchestrator/src/schemas/volumes.schemas.ts:28-51: every host/gpu field
// is independently nullish).
func TestCompleteImportOmitsUnreportedHostAndGPUFields(t *testing.T) {
	var gotRaw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotRaw)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"volume_id":"vol-1","warnings":[]}}`)
	}))
	defer srv.Close()

	hostname := "gpu-7f2a"
	vendor := "nvidia"
	if _, err := NewAuthed(srv.URL, "tok", "t").CompleteImport(ImportCompleteRequest{
		VolumeID:       "vol-1",
		ImportToken:    "tok-1",
		OgreSnapshotID: "snap-1",
		Observation: ImportObservation{
			Schema: ImportObservationSchema,
			// Hostname reported; every other Host field is not (a real
			// shape: ogre could identify the box but not its kernel).
			Host: ImportHost{Hostname: &hostname},
			// Vendor reported; Name/Count/etc. are not (a GPU is visible
			// but ogre couldn't read its driver details).
			GPU: ImportGPU{Vendor: &vendor},
		},
	}); err != nil {
		t.Fatalf("CompleteImport: %v", err)
	}

	observation, ok := gotRaw["observation"].(map[string]any)
	if !ok {
		t.Fatalf("body has no \"observation\" object: %#v", gotRaw)
	}
	host, ok := observation["host"].(map[string]any)
	if !ok {
		t.Fatalf("observation has no \"host\" object: %#v", observation)
	}
	if host["hostname"] != "gpu-7f2a" {
		t.Errorf("host.hostname = %v, want gpu-7f2a", host["hostname"])
	}
	for _, key := range []string{"os", "kernel", "cpu_cores", "memory_gb", "storage_gb"} {
		if _, present := host[key]; present {
			t.Errorf("host.%s must be ABSENT (ogre never reported it), got it present: %+v", key, host)
		}
	}

	gpu, ok := observation["gpu"].(map[string]any)
	if !ok {
		t.Fatalf("observation has no \"gpu\" object: %#v", observation)
	}
	if gpu["vendor"] != "nvidia" {
		t.Errorf("gpu.vendor = %v, want nvidia", gpu["vendor"])
	}
	for _, key := range []string{"name", "count", "driver_cuda", "toolkit_cuda", "rocm_version", "compute_cap", "skew"} {
		if _, present := gpu[key]; present {
			t.Errorf("gpu.%s must be ABSENT (ogre never reported it), got it present: %+v", key, gpu)
		}
	}
}

// TestStartImportDecodesCredentials checks POST /volumes/import/start
// decodes a real-shaped response.
func TestStartImportDecodesCredentials(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{
			"volume_id":"vol-1","storage_prefix":"teams/t1/volumes/vol-1","restic_password":"pw",
			"restic_backup_id":"repo","import_token":"tok-1","expires_at":"2026-09-27T00:00:00Z",
			"credentials":{"endpoint":"https://s3.example.com","bucket":"b","access_key_id":"ak","secret_access_key":"sk","region":"us-east-1"}
		}}`)
	}))
	defer srv.Close()

	got, err := NewAuthed(srv.URL, "tok", "t").StartImport(ImportStartRequest{Name: "my-box"})
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if gotPath != "/volumes/import/start" {
		t.Errorf("path = %q, want /volumes/import/start", gotPath)
	}
	if got.VolumeID != "vol-1" || got.ResticBackupID != "repo" {
		t.Errorf("result = %+v", got)
	}
}
