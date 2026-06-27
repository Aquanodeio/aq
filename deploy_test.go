package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/config"
)

// deployServer is a minimal fake of the orchestrator endpoints `aq deploy` calls.
type deployServer struct {
	keys          []map[string]any
	deployBody    DeployBodyCapture
	statusReadyAt int32
	statusPolls   int32
	lastTeamID    string
	appURL        string // app_url on the deployment row once ACTIVE (e.g. http://ip:22)
	restoreStatus string // restore_status on the deployment row once ACTIVE (#235)
	restoreError  string // restore_error detail accompanying a non-success restore_status
	// withServiceURL controls whether the ACTIVE row also publishes a template
	// service URL. A failed restore skips the app start server-side, so the URL
	// never appears; set false to exercise that path.
	noServiceURL bool
}

type DeployBodyCapture struct {
	SnapshotSource string
	SSHKeyID       string
	Template       string
	GPUModel       string
	Provider       string
}

func (s *deployServer) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, s.keys)
	})

	mux.HandleFunc("/deployments/deploy-snapshot", func(w http.ResponseWriter, r *http.Request) {
		s.lastTeamID = r.Header.Get("x-team-id")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.deployBody = DeployBodyCapture{
			SnapshotSource: str(body["snapshotSource"]),
			SSHKeyID:       str(body["sshKeyId"]),
			Template:       str(body["template"]),
			GPUModel:       str(body["gpuModel"]),
			Provider:       str(body["provider"]),
		}
		writeData(w, map[string]any{
			"deploymentId": 5151,
			"projectId":    "proj-d",
			"status":       "PENDING",
		})
	})

	mux.HandleFunc("/deployments/5151/status", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&s.statusPolls, 1)
		dep := map[string]any{"id": 5151, "status": "PENDING", "app_url": ""}
		if n >= s.statusReadyAt {
			dep["status"] = "ACTIVE"
			dep["app_url"] = s.appURL
			if s.restoreStatus != "" {
				dep["restore_status"] = s.restoreStatus
			}
			if s.restoreError != "" {
				dep["restore_error"] = s.restoreError
			}
			if !s.noServiceURL {
				dep["service_credentials"] = map[string]any{
					"template": "comfyui",
					"url":      "https://comfy.box.aquanode.io",
					"username": "admin",
					"password": "s3cr3t",
					"status":   "running",
				}
			}
		}
		writeData(w, map[string]any{"deploymentId": 5151, "status": dep["status"], "deployment": dep})
	})

	return mux
}

func TestRunDeployHappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop")
	server := &deployServer{
		keys:          []map[string]any{{"id": "key-existing", "name": "laptop", "public_key": "ssh-ed25519 AAAA laptop"}},
		statusReadyAt: 2,
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}

	var out bytes.Buffer
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-42",
		template:     templateComfyUI,
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("runDeploy error: %v", err)
	}

	if server.deployBody.SnapshotSource != "ext-42" {
		t.Errorf("snapshot source not forwarded: got %q", server.deployBody.SnapshotSource)
	}
	if server.deployBody.SSHKeyID != "key-existing" {
		t.Errorf("ssh key not forwarded: got %q", server.deployBody.SSHKeyID)
	}
	if server.deployBody.Template != templateComfyUI {
		t.Errorf("template not forwarded: got %q", server.deployBody.Template)
	}
	if server.lastTeamID != "team-1" {
		t.Errorf("x-team-id header not sent: got %q", server.lastTeamID)
	}
	got := out.String()
	if !strings.Contains(got, "https://comfy.box.aquanode.io") {
		t.Errorf("output missing HTTPS URL; got:\n%s", got)
	}
}

func TestRunDeployRestoreOnlyReportsActive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	server := &deployServer{
		keys:          []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}},
		statusReadyAt: 1,
		appURL:        "http://203.0.113.7:22",
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-7",
		template:     "", // restore only
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runDeploy error: %v", err)
	}
	if server.deployBody.Template != "" {
		t.Errorf("expected no template for restore-only deploy, got %q", server.deployBody.Template)
	}
	got := out.String()
	if !strings.Contains(got, "restored onto deployment #5151") {
		t.Errorf("expected restore-only ready message; got:\n%s", got)
	}
	// #209: a restore-only deploy prints the box IP + ssh line after ACTIVE.
	if !strings.Contains(got, "203.0.113.7") || !strings.Contains(got, "ssh root@203.0.113.7") {
		t.Errorf("expected IP + ssh connection info; got:\n%s", got)
	}
}

// TestRunDeployRestoreOnlyFailsOnFailedRestore is the #235 regression: a
// restore-only deploy whose server-side restore FAILED must NOT print success —
// it returns a clear error (non-zero exit) even though the box reached ACTIVE.
func TestRunDeployRestoreOnlyFailsOnFailedRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	server := &deployServer{
		keys:          []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}},
		statusReadyAt: 1,
		appURL:        "http://203.0.113.7:22",
		restoreStatus: "FAILED",
		restoreError:  "repository does not exist",
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-124",
		template:     "", // restore only → waitForActive
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err == nil {
		t.Fatalf("expected a restore-failure error, got nil; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "restore failed") || !strings.Contains(err.Error(), "repository does not exist") {
		t.Errorf("error should explain the failed restore; got: %v", err)
	}
	if strings.Contains(out.String(), "was restored") {
		t.Errorf("must not print a success message on a failed restore; got:\n%s", out.String())
	}
}

// TestRunDeployWithTemplateFailsOnFailedRestore covers the #235 template path:
// when the restore fails the orchestrator skips the app start, so the service URL
// never appears — the CLI must abort on the recorded FAILED status instead of
// spinning out the timeout.
func TestRunDeployWithTemplateFailsOnFailedRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	server := &deployServer{
		keys:          []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}},
		statusReadyAt: 1,
		restoreStatus: "FAILED",
		restoreError:  "repository does not exist",
		noServiceURL:  true, // failed restore → app start skipped → URL never appears
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-124",
		template:     templateComfyUI,
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second, // must abort well before this
	})
	if err == nil {
		t.Fatalf("expected a restore-failure error, got nil; output:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "restore failed") {
		t.Errorf("error should explain the failed restore; got: %v", err)
	}
	if strings.Contains(out.String(), "is live") {
		t.Errorf("must not print a service-ready message on a failed restore; got:\n%s", out.String())
	}
}

// TestRunDeployFailsOnPartialRestore: a partial restore (some paths missing) is
// also surfaced as a non-zero error rather than a clean success (#235).
func TestRunDeployFailsOnPartialRestore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	server := &deployServer{
		keys:          []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}},
		statusReadyAt: 1,
		appURL:        "http://203.0.113.7:22",
		restoreStatus: "PARTIAL",
		restoreError:  "restored 1 of 2 path(s)",
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-9",
		template:     "",
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected an incomplete-restore error, got: %v", err)
	}
}

// TestRunDeployRestoreOnlySucceedsOnSuccessStatus confirms an explicit SUCCESS
// restore_status still prints the ready message and exits 0 (#235).
func TestRunDeployRestoreOnlySucceedsOnSuccessStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	server := &deployServer{
		keys:          []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}},
		statusReadyAt: 1,
		appURL:        "http://203.0.113.7:22",
		restoreStatus: "SUCCESS",
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-7",
		template:     "",
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runDeploy error: %v", err)
	}
	if !strings.Contains(out.String(), "restored onto deployment #5151") {
		t.Errorf("expected the restore-ready message; got:\n%s", out.String())
	}
}

func TestRunDeployFailsWhenDeploymentCloses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	mux := http.NewServeMux()
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}})
	})
	mux.HandleFunc("/deployments/deploy-snapshot", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 5151, "projectId": "p", "status": "PENDING"})
	})
	mux.HandleFunc("/deployments/5151/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 5151, "status": "FAILED",
			"deployment": map[string]any{"id": 5151, "status": "FAILED"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-1",
		template:     templateComfyUI,
		out:          &bytes.Buffer{},
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "FAILED") {
		t.Fatalf("expected failure error, got: %v", err)
	}
}

// TestRunDeployAbortsOnPermanentStatusError is the #208 regression for the
// restore-only (waitForActive) path: a permanent hard 4xx aborts fast instead of
// spinning until the timeout.
func TestRunDeployAbortsOnPermanentStatusError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	mux := http.NewServeMux()
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}})
	})
	mux.HandleFunc("/deployments/deploy-snapshot", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 5151, "projectId": "p", "status": "PENDING"})
	})
	mux.HandleFunc("/deployments/5151/status", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-1",
		template:     "", // restore only → waitForActive
		out:          &bytes.Buffer{},
		pollInterval: 2 * time.Millisecond,
		timeout:      time.Hour,
	})
	if err == nil || !strings.Contains(err.Error(), "could not check deployment 5151 status") {
		t.Fatalf("expected fast abort on permanent 4xx, got: %v", err)
	}
}

func TestDeployRequiresSnapshot(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	_ = config.Save(&config.Credential{APIURL: "http://x", Token: "aq_sk", TeamID: "t"})
	err := deploy(nil)
	if err == nil || !strings.Contains(err.Error(), "snapshot is required") {
		t.Fatalf("expected snapshot-required error, got: %v", err)
	}
}

func TestDeployRequiresLogin(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := deploy([]string{"--snapshot", "ext-1"})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in error, got: %v", err)
	}
}

func TestDeployRejectsConflictingTemplateFlags(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	_ = config.Save(&config.Credential{APIURL: "http://x", Token: "aq_sk", TeamID: "t"})
	err := deploy([]string{"--snapshot", "ext-1", "--comfyui", "--jupyter"})
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("expected mutually-exclusive error, got: %v", err)
	}
}

func TestDeployAcceptsPositionalSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	server := &deployServer{
		keys:          []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}},
		statusReadyAt: 1,
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runDeploy(deployOptions{
		cred:         cred,
		snapshot:     "ext-99",
		template:     templateJupyter,
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runDeploy error: %v", err)
	}
	if server.deployBody.SnapshotSource != "ext-99" || server.deployBody.Template != templateJupyter {
		t.Errorf("unexpected deploy body: %+v", server.deployBody)
	}
}
