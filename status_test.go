package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func TestRunStatusReadyShowsURLAndCreds(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	var gotAPIKey, gotTeamID string
	mux.HandleFunc("/deployments/4242/status", func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotTeamID = r.Header.Get("x-team-id")
		dep := map[string]any{
			"id":     4242,
			"status": "ACTIVE",
			"service_credentials": map[string]any{
				"template": "comfyui",
				"url":      "https://comfy.box.aquanode.io",
				"username": "admin",
				"password": "s3cr3t",
				"status":   "running",
			},
		}
		writeData(w, map[string]any{"deploymentId": 4242, "status": "ACTIVE", "deployment": dep})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, target: "4242", out: &out, errOut: &errOut}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}

	got := out.String()
	for _, want := range []string{"ACTIVE", "https://comfy.box.aquanode.io", "admin"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q; got:\n%s", want, got)
		}
	}
	// Password must not be echoed to stdout by default (ticket #204).
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("password leaked into stdout; got:\n%s", got)
	}
	if !strings.Contains(errOut.String(), "--show-secrets") {
		t.Errorf("stderr missing the --show-secrets pointer; got:\n%s", errOut.String())
	}
	if gotAPIKey != "aq_sk_test" || gotTeamID != "team-1" {
		t.Errorf("auth headers not sent: key=%q team=%q", gotAPIKey, gotTeamID)
	}
}

func TestRunStatusShowSecretsEchoesPassword(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/4242/status", func(w http.ResponseWriter, r *http.Request) {
		dep := map[string]any{
			"id":     4242,
			"status": "ACTIVE",
			"service_credentials": map[string]any{
				"template": "comfyui",
				"url":      "https://comfy.box.aquanode.io",
				"username": "admin",
				"password": "s3cr3t",
				"status":   "running",
			},
		}
		writeData(w, map[string]any{"deploymentId": 4242, "status": "ACTIVE", "deployment": dep})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, target: "4242", showSecrets: true, out: &out, errOut: &errOut}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}
	if !strings.Contains(out.String(), "s3cr3t") {
		t.Errorf("--show-secrets should echo the password to stdout; got:\n%s", out.String())
	}
}

func TestRunStatusStillProvisioning(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/7/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 7, "status": "PENDING",
			"deployment": map[string]any{"id": 7, "status": "PENDING"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, target: "7", out: &out}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "PENDING") || !strings.Contains(got, "Still provisioning") {
		t.Errorf("expected still-provisioning output; got:\n%s", got)
	}
}

// TestRunStatusActiveRestoreOnlyShowsConnectionInfo is the #213 fix: an
// ACTIVE/RUNNING box with no service credentials (a restore-only deploy) reports
// as ready with the box IP + ssh line instead of "Still provisioning" forever.
func TestRunStatusActiveRestoreOnlyShowsConnectionInfo(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/909/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 909, "status": "ACTIVE",
			"deployment": map[string]any{"id": 909, "status": "ACTIVE", "app_url": "http://203.0.113.7:22"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, target: "909", out: &out}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "Still provisioning") {
		t.Errorf("active restore-only box should not say still provisioning; got:\n%s", got)
	}
	for _, want := range []string{"is ready", "203.0.113.7", "aq ssh 909", "aq-909"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q; got:\n%s", want, got)
		}
	}
}

// TestRunStatusActiveRestoreOnlyNoAppURL covers an ACTIVE box whose row has no
// app_url yet: it still reports ready (no provisioning message) but omits the
// connection lines rather than printing a blank IP.
func TestRunStatusActiveRestoreOnlyNoAppURL(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/910/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 910, "status": "RUNNING",
			"deployment": map[string]any{"id": 910, "status": "RUNNING"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, target: "910", out: &out}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "Still provisioning") {
		t.Errorf("active box should not say still provisioning; got:\n%s", got)
	}
	if !strings.Contains(got, "is ready") {
		t.Errorf("expected ready message; got:\n%s", got)
	}
	if strings.Contains(got, "ssh root@") {
		t.Errorf("expected no ssh line without app_url; got:\n%s", got)
	}
}

func TestStatusRequiresDeploymentID(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := status(nil)
	if err == nil || !strings.Contains(err.Error(), "deployment id is required") {
		t.Fatalf("expected missing-id error, got: %v", err)
	}
}

// TestRunStatusResolvesProjectID is the #209 fix: a non-numeric token is treated
// as a project id and resolved to its current deployment via the project route,
// then status is fetched for the resolved deployment id.
func TestRunStatusResolvesProjectID(t *testing.T) {
	const projectID = "11111111-2222-3333-4444-555555555555"
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/project/"+projectID, func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": 4242, "status": "ACTIVE"})
	})
	mux.HandleFunc("/deployments/4242/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 4242, "status": "PENDING",
			"deployment": map[string]any{"id": 4242, "status": "PENDING"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, target: projectID, out: &out}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}
	if !strings.Contains(out.String(), "Deployment #4242") {
		t.Errorf("expected resolved deployment #4242; got:\n%s", out.String())
	}
}

// TestRunStatusUnknownProjectIDExplains checks that an unresolvable token yields
// a message pointing the user at the numeric deployment id (#209).
func TestRunStatusUnknownProjectIDExplains(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/project/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "not found"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runStatus(statusOptions{cred: cred, target: "not-a-real-id", out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "numeric deployment id") {
		t.Fatalf("expected numeric-deployment-id hint, got: %v", err)
	}
}

func TestStatusRequiresLogin(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := status([]string{"4242"})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in error, got: %v", err)
	}
}

// TestFormatPodSaveStateNoVolume checks a bare pod (D4: no volume attached)
// reads as its own distinct fact, never "never saved": that phrase implies a
// volume that should have saved and didn't, which is not what "no volume"
// means.
func TestFormatPodSaveStateNoVolume(t *testing.T) {
	if got := formatPodSaveState(nil, time.Now()); got != "no volume attached" {
		t.Errorf("got %q, want %q", got, "no volume attached")
	}
}

// TestFormatPodSaveStateNeverSavedWhenHeadSavedAtNil is the honesty
// constraint the retired SnapshotHistory-based lookup used to get backwards:
// a volume that genuinely has never landed a save reads "never saved" from
// HeadSavedAt being nil, not from a wrong endpoint that could never see this
// pod's saves at all.
func TestFormatPodSaveStateNeverSavedWhenHeadSavedAtNil(t *testing.T) {
	v := &api.SetupVolumeSummary{SaveState: "unknown", HeadSavedAt: nil}
	if got := formatPodSaveState(v, time.Now()); got != "never saved" {
		t.Errorf("got %q, want %q", got, "never saved")
	}
}

// TestFormatPodSaveStateSavedShowsAge is the actual regression: a volume with
// a real, recent HeadSavedAt (confirmed live on deployments 3807 and 3809,
// both of which `aq status` printed "never saved" for before this fix) must
// show its true age, not a blanket "never saved".
func TestFormatPodSaveStateSavedShowsAge(t *testing.T) {
	now := time.Date(2026, 9, 26, 20, 36, 46, 0, time.UTC)
	saved := "2026-09-26T20:06:46.999Z"
	v := &api.SetupVolumeSummary{SaveState: "saved", HeadSavedAt: &saved}
	if got := formatPodSaveState(v, now); got != "30m ago" {
		t.Errorf("got %q, want %q", got, "30m ago")
	}
}

// TestFormatPodSaveStateFailingShowsReason checks the failing branch reports
// the age of the last GOOD save plus the reason it's currently failing,
// never silently reading as "saved" (the three-state signal rule: "unknown"/
// "failing" must never collapse into the reassuring state).
func TestFormatPodSaveStateFailingShowsReason(t *testing.T) {
	now := time.Date(2026, 9, 26, 21, 0, 0, 0, time.UTC)
	saved := "2026-09-26T20:00:00.000Z"
	reason := "agent unreachable"
	v := &api.SetupVolumeSummary{SaveState: "failing", HeadSavedAt: &saved, LastSaveError: &reason}
	got := formatPodSaveState(v, now)
	if !strings.Contains(got, "failed") || !strings.Contains(got, reason) {
		t.Errorf("got %q, want it to mention \"failed\" and %q", got, reason)
	}
}

// TestPodSaveLabelDegradesToUnknownWithNoSetupID checks a deployment with no
// SetupID (predates the pod/environment/volume model, or the field failed to
// resolve) degrades gracefully instead of guessing.
func TestPodSaveLabelDegradesToUnknownWithNoSetupID(t *testing.T) {
	client := api.NewAuthed("http://unused.invalid", "tok", "team-1")
	if got := podSaveLabel(client, "", time.Now()); got != "unknown" {
		t.Errorf("got %q, want %q", got, "unknown")
	}
}

// TestRunStatusShowsRealSaveState is the end-to-end regression: `aq status`
// must resolve the deployment's setup_id, fetch the pod, and print its real
// Volume.headSavedAt age, not "never saved" from a retired endpoint that
// never saw this pod's saves in the first place.
func TestRunStatusShowsRealSaveState(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/3807/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 3807, "status": "CLOSED",
			"deployment": map[string]any{"id": 3807, "status": "CLOSED", "setup_id": "b8591f0f-038e-4d48-b9a2-26a3b7caeadd"}})
	})
	mux.HandleFunc("/setups/b8591f0f-038e-4d48-b9a2-26a3b7caeadd", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"id": "b8591f0f-038e-4d48-b9a2-26a3b7caeadd", "name": "MI300X box 11", "status": "ready",
			"attachedDeploymentId": nil, "stopping": false,
			"volume": map[string]any{
				"id": "47a69ae8-f1d5-418f-8687-928bb4c464ff", "name": "MI300X box 11",
				"sizeBytes": 8270434, "headSavedAt": "2026-09-26T20:06:46.999Z",
				"saveState": "saved", "lastSaveError": nil,
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, target: "3807", out: &out}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "never saved") {
		t.Errorf("expected the real save age, not \"never saved\"; got:\n%s", got)
	}
	if !strings.Contains(got, "Last saved:") || !strings.Contains(got, "ago") {
		t.Errorf("expected a \"Last saved: ... ago\" line; got:\n%s", got)
	}
}
