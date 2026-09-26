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

// TestVolumeAttachedLabelIsThreeState checks unattached, attached-and-running,
// and attached-but-stopped all render differently: a bare boolean would
// collapse the last two into the same "yes", which is exactly the state a
// user needs distinguished (a stopped pod's volume can still be restored;
// a running one's can't).
func TestVolumeAttachedLabelIsThreeState(t *testing.T) {
	podID, podName := "pod-1", "trainer"
	cases := []struct {
		name string
		v    api.Volume
		want string
	}{
		{"unattached", api.Volume{}, "-"},
		{"running", api.Volume{AttachedPodID: &podID, AttachedPodName: &podName, Running: true}, "trainer"},
		{"stopped", api.Volume{AttachedPodID: &podID, AttachedPodName: &podName, Running: false}, "trainer (stopped)"},
		{"no name falls back to id", api.Volume{AttachedPodID: &podID, Running: true}, "pod-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumeAttachedLabel(tc.v); got != tc.want {
				t.Errorf("volumeAttachedLabel(%+v) = %q, want %q", tc.v, got, tc.want)
			}
		})
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
// the whole list, including whether each one is attached.
func TestRunVolumeLsWithNoTargetListsAll(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "vol-1", "name": "research-data", "sizeBytes": 1073741824, "attachedPodId": "pod-1", "attachedPodName": "trainer", "running": true, "headSavedAt": "2026-09-26T00:00:00Z", "saveState": "saved"},
			{"id": "vol-2", "name": "scratch", "sizeBytes": nil, "attachedPodId": nil, "attachedPodName": nil, "running": false, "headSavedAt": nil, "saveState": "unknown"},
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
}

// TestRunVolumeLsWithTargetShowsDetailAndHistory checks `aq volume ls <id>`
// resolves by name and renders the point history with provenance and label.
func TestRunVolumeLsWithTargetShowsDetailAndHistory(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "vol-1", "name": "research-data"}})
	})
	mux.HandleFunc("/volumes/vol-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"id": "vol-1", "name": "research-data", "sizeBytes": 1073741824,
			"mountPath": "/workspace", "attachedPodId": "pod-1", "attachedPodName": "trainer",
			"running": true, "headSavedAt": "2026-09-26T00:00:00Z", "saveState": "saved",
			"lastSaveError": nil, "createdAt": "2026-09-01T00:00:00Z",
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
	for _, want := range []string{"research-data", "Attached to pod: trainer", "stop"} {
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

// TestRunVolumeRmSurfacesConflictWhileAttached checks a 409 from the
// orchestrator surfaces as a real error rather than a false "deleted".
func TestRunVolumeRmSurfacesConflictWhileAttached(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/volumes", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "vol-1", "name": "research-data"}})
	})
	mux.HandleFunc("/volumes/vol-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"error":"volume is attached to a running pod"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runVolumeRm(volumeRmOptions{cred: cred, target: "research-data", out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "attached to a running pod") {
		t.Fatalf("expected the attached-conflict error surfaced, got: %v", err)
	}
}
