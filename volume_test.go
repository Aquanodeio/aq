package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// TestVolumeStatusLabelIsFourState checks copying, in-use, never-saved, and
// idle all render differently: a bare attached/unattached boolean would
// collapse "in use by a running pod" and "referenced by a stopped one" into
// the same state, which is exactly the distinction a user needs (a stopped
// pod's volume can still be picked up, rolled back, or deleted; a running
// one's can't).
func TestVolumeStatusLabelIsFourState(t *testing.T) {
	podID, podName := "pod-1", "trainer"
	cases := []struct {
		name string
		v    api.Volume
		want string
	}{
		{"idle", api.Volume{SaveState: "saved"}, "idle"},
		{"never saved", api.Volume{SaveState: "never_saved"}, "never saved"},
		{"copying wins over never saved", api.Volume{SaveState: "never_saved", Copying: true}, "copying"},
		{"in use by name", api.Volume{InUseBy: &api.VolumeHolder{PodID: &podID, PodName: &podName}}, "in use by trainer"},
		{"in use, name unknown", api.Volume{InUseBy: &api.VolumeHolder{PodID: &podID}}, "in use by another pod"},
		{"in use wins over never saved", api.Volume{SaveState: "never_saved", InUseBy: &api.VolumeHolder{PodID: &podID, PodName: &podName}}, "in use by trainer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumeStatusLabel(tc.v); got != tc.want {
				t.Errorf("volumeStatusLabel(%+v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestSaveStateLabelNeverSavedVsUnknown checks the wire's "never_saved"
// renders as the readable "never saved", while an empty string (a backend
// too old to send the field) renders as the CLI's own "unknown" -- the two
// must never collapse into the same string.
func TestSaveStateLabelNeverSavedVsUnknown(t *testing.T) {
	cases := map[string]string{
		"":            "unknown",
		"never_saved": "never saved",
		"saved":       "saved",
		"failing":     "failing",
	}
	for raw, want := range cases {
		if got := saveStateLabel(raw); got != want {
			t.Errorf("saveStateLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestVolumeDispatchesToKnownSubcommands mirrors TestEnvDispatchesToKnownSubcommands
// for `aq volume`.
func TestVolumeDispatchesToKnownSubcommands(t *testing.T) {
	if err := volume(nil); err == nil || !strings.Contains(err.Error(), "usage: aq volume") {
		t.Errorf("expected a usage error with no args, got: %v", err)
	}
	if err := volume([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), `unknown subcommand "bogus"`) {
		t.Errorf("expected an unknown-subcommand error, got: %v", err)
	}
}

// TestRunVolumeLsWithNoTargetListsAll checks a bare `aq volume ls` renders
// the whole list, including each one's status.
func TestRunVolumeLsWithNoTargetListsAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "vol-1", "name": "research-data", "sizeBytes": 1073741824, "inUseBy": map[string]any{"podId": "pod-1", "podName": "trainer"}, "copying": false, "headSavedAt": "2026-09-26T00:00:00Z", "saveState": "saved"},
			{"id": "vol-2", "name": "scratch", "sizeBytes": nil, "inUseBy": nil, "copying": false, "headSavedAt": nil, "saveState": "never_saved"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runVolumeLs(volumeLsOptions{cred: cred, out: &out}); err != nil {
		t.Fatalf("runVolumeLs: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "research-data") || !strings.Contains(got, "scratch") {
		t.Errorf("expected both volumes listed; got:\n%s", got)
	}
	if !strings.Contains(got, "in use by trainer") {
		t.Errorf("expected the in-use volume's holder printed; got:\n%s", got)
	}
	if !strings.Contains(got, "never saved") {
		t.Errorf("expected the unsaved volume's status printed; got:\n%s", got)
	}
}

// TestRunVolumeLsWithTargetShowsDetailAndHistory checks `aq volume ls <id>`
// resolves by name and renders who uses it plus the point history with
// provenance and label.
func TestRunVolumeLsWithTargetShowsDetailAndHistory(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "vol-1", "name": "research-data"}})
	})
	mux.HandleFunc("/volumes/vol-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"id": "vol-1", "name": "research-data", "sizeBytes": 1073741824,
			"mountPath": "/workspace", "inUseBy": map[string]any{"podId": "pod-1", "podName": "trainer"},
			"copying": false, "headSavedAt": "2026-09-26T00:00:00Z", "saveState": "saved",
			"lastSaveError": nil, "createdAt": "2026-09-01T00:00:00Z",
			"usedBy": []map[string]any{
				{"podId": "pod-1", "podName": "trainer", "state": "running"},
			},
			"points": []map[string]any{
				{"id": "pt-1", "createdAt": "2026-09-25T00:00:00Z", "provenance": "stop", "label": nil},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runVolumeLs(volumeLsOptions{cred: cred, target: "research-data", out: &out}); err != nil {
		t.Fatalf("runVolumeLs: %v", err)
	}
	got := out.String()
	for _, want := range []string{"research-data", "Status: in use by trainer", "trainer (running)", "stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// TestRunVolumeDupPostsNewName checks `aq volume dup` resolves the source by
// name and posts the new name.
func TestRunVolumeDupPostsNewName(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "vol-1", "name": "research-data"}})
	})
	var gotPath string
	mux.HandleFunc("/volumes/vol-1/duplicate", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeData(w, map[string]any{"id": "vol-3", "name": "research-data-copy"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runVolumeDup(volumeDupOptions{cred: cred, target: "research-data", name: "research-data-copy", out: &out}); err != nil {
		t.Fatalf("runVolumeDup: %v", err)
	}
	if gotPath != "/volumes/vol-1/duplicate" {
		t.Errorf("path = %q, want /volumes/vol-1/duplicate", gotPath)
	}
	if !strings.Contains(out.String(), "research-data-copy") {
		t.Errorf("expected the new name printed; got:\n%s", out.String())
	}
}

// TestRunVolumeRestoreSendsThePointID checks `aq volume restore` never
// interprets its argument as anything but the exact point given.
func TestRunVolumeRestoreSendsThePointID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "vol-1", "name": "research-data"}})
	})
	mux.HandleFunc("/volumes/vol-1/restore", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": "vol-1", "name": "research-data"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runVolumeRestore(volumeRestoreOptions{cred: cred, target: "research-data", pointID: "pt-0", out: &out}); err != nil {
		t.Fatalf("runVolumeRestore: %v", err)
	}
	if !strings.Contains(out.String(), "pt-0") {
		t.Errorf("expected the point id printed; got:\n%s", out.String())
	}
}

// TestRunVolumeRmSurfacesInUseConflict checks a 409 from the orchestrator
// (a pod's live box has the volume) surfaces as a real error rather than a
// false "deleted".
func TestRunVolumeRmSurfacesInUseConflict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "vol-1", "name": "research-data"}})
	})
	mux.HandleFunc("/volumes/vol-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error":"Volume research-data is in use by pod trainer. Stop trainer first.","data":{"code":"PLACEMENT_VOLUME_IN_USE","inUseBy":{"podId":"pod-1","podName":"trainer"}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runVolumeRm(volumeRmOptions{cred: cred, target: "research-data", out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "in use by pod trainer") {
		t.Fatalf("expected the in-use-conflict error surfaced, got: %v", err)
	}
}

// TestRunVolumeRmPrintsDetachedPods checks a successful delete prints every
// stopped pod the delete detached, so the caller knows which pods just lost
// their volume.
func TestRunVolumeRmPrintsDetachedPods(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "vol-1", "name": "research-data"}})
	})
	mux.HandleFunc("/volumes/vol-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"id": "vol-1",
			"detachedPods": []map[string]any{
				{"podId": "pod-2", "podName": "trainer-b"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runVolumeRm(volumeRmOptions{cred: cred, target: "research-data", out: &out}); err != nil {
		t.Fatalf("runVolumeRm: %v", err)
	}
	if !strings.Contains(out.String(), "Detached from trainer-b") {
		t.Errorf("expected the detached pod printed; got:\n%s", out.String())
	}
}
