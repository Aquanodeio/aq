package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// upServer is a minimal fake of the orchestrator funnel endpoints `aq up` calls.
type upServer struct {
	keys          []map[string]any // existing ssh keys
	created       map[string]any   // last created key body
	upBody        UpBodyCapture
	statusReadyAt int32 // status polls before the service URL appears
	statusPolls   int32
	lastAPIKey    string
	lastTeamID    string
}

type UpBodyCapture struct {
	Template string
	SSHKeyID string
	GPUModel string
	Provider string
}

func (s *upServer) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		s.lastAPIKey = r.Header.Get("x-api-key")
		if r.Method == http.MethodGet {
			writeData(w, s.keys)
			return
		}
		// POST — register a key and echo it back with an id.
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.created = body
		writeData(w, map[string]any{
			"id":         "key-new",
			"name":       body["name"],
			"public_key": body["public_key"],
		})
	})

	mux.HandleFunc("/deployments/up", func(w http.ResponseWriter, r *http.Request) {
		s.lastAPIKey = r.Header.Get("x-api-key")
		s.lastTeamID = r.Header.Get("x-team-id")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.upBody = UpBodyCapture{
			Template: str(body["template"]),
			SSHKeyID: str(body["sshKeyId"]),
			GPUModel: str(body["gpuModel"]),
			Provider: str(body["provider"]),
		}
		writeData(w, map[string]any{
			"deploymentId": 4242,
			"projectId":    "proj-1",
			"status":       "PENDING",
		})
	})

	mux.HandleFunc("/deployments/4242/status", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&s.statusPolls, 1)
		dep := map[string]any{"id": 4242, "status": "PENDING", "app_url": ""}
		if n >= s.statusReadyAt {
			dep["status"] = "ACTIVE"
			dep["service_credentials"] = map[string]any{
				"template": "comfyui",
				"url":      "https://comfy.box.aquanode.io",
				"username": "admin",
				"password": "s3cr3t",
				"status":   "running",
			}
		}
		writeData(w, map[string]any{"deploymentId": 4242, "status": dep["status"], "deployment": dep})
	})

	return mux
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// writeFakePubKey drops an id_ed25519.pub holding content under $HOME/.ssh so
// readLocalPublicKey finds a deterministic local key. Tests set $HOME to a temp
// dir first so this never touches the real machine's keys.
func writeFakePubKey(t *testing.T, content string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "id_ed25519.pub"), []byte(content+"\n"), 0o600); err != nil {
		t.Fatalf("write pubkey: %v", err)
	}
}

func TestRunUpHappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// The local key matches the registered one (comment differs) — aq should
	// reuse the registered key, not register a duplicate.
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@thismachine")

	server := &upServer{
		keys:          []map[string]any{{"id": "key-existing", "name": "laptop", "public_key": "ssh-ed25519 AAAA laptop"}},
		statusReadyAt: 2,
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}

	var out bytes.Buffer
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		gpuModel:     "RTX 4090",
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("runUp error: %v", err)
	}

	if server.created != nil {
		t.Errorf("should reuse the matching registered key, but registered a new one: %v", server.created)
	}
	if server.upBody.SSHKeyID != "key-existing" {
		t.Errorf("up should reuse the matching key id, got %q", server.upBody.SSHKeyID)
	}
	got := out.String()
	if !strings.Contains(got, "Using your registered SSH key") {
		t.Errorf("output should report which key was used; got:\n%s", got)
	}
	if !strings.Contains(got, "https://comfy.box.aquanode.io") {
		t.Errorf("output missing HTTPS URL; got:\n%s", got)
	}
	if !strings.Contains(got, "s3cr3t") || !strings.Contains(got, "admin") {
		t.Errorf("output missing credentials; got:\n%s", got)
	}
}

// TestRunUpRegistersWhenNoRegisteredKeyMatches is the #203 regression: the
// account already has a key, but it isn't the user's — aq must register the
// local key instead of provisioning the box with an unusable one.
func TestRunUpRegistersWhenNoRegisteredKeyMatches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAAMINE me@laptop")

	server := &upServer{
		// A teammate's key — present but not the user's.
		keys:          []map[string]any{{"id": "key-teammate", "name": "alice", "public_key": "ssh-ed25519 AAAATHEIRS alice@box"}},
		statusReadyAt: 1,
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runUp error: %v", err)
	}
	if server.created == nil {
		t.Fatal("expected the local key to be registered, but none was")
	}
	if got := str(server.created["public_key"]); got != "ssh-ed25519 AAAAMINE me@laptop" {
		t.Errorf("registered the wrong key body: %q", got)
	}
	if server.upBody.SSHKeyID != "key-new" {
		t.Errorf("up should use the newly-registered key id, not the teammate's; got %q", server.upBody.SSHKeyID)
	}
}

func TestRunUpRegistersSSHKeyWhenNoneExist(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Seed a local public key under the fake HOME.
	writeFakePubKey(t, "ssh-ed25519 AAAAFAKE laptop")

	server := &upServer{keys: []map[string]any{}, statusReadyAt: 1}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runUp(upOptions{
		cred:         cred,
		template:     templateJupyter,
		out:          &out,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runUp error: %v", err)
	}
	if server.created == nil {
		t.Fatal("expected an SSH key to be registered")
	}
	if server.upBody.SSHKeyID != "key-new" {
		t.Errorf("up should use the newly-registered key id, got %q", server.upBody.SSHKeyID)
	}
	if server.upBody.Template != templateJupyter {
		t.Errorf("template not forwarded: got %q", server.upBody.Template)
	}
	if server.lastTeamID != "team-1" {
		t.Errorf("x-team-id header not sent: got %q", server.lastTeamID)
	}
}

func TestRunUpFailsWhenDeploymentCloses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")

	mux := http.NewServeMux()
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}})
	})
	mux.HandleFunc("/deployments/up", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 7, "projectId": "p", "status": "PENDING"})
	})
	mux.HandleFunc("/deployments/7/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 7, "status": "FAILED",
			"deployment": map[string]any{"id": 7, "status": "FAILED"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		out:          &bytes.Buffer{},
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "FAILED") {
		t.Fatalf("expected failure error, got: %v", err)
	}
}

func TestMatchRegisteredKey(t *testing.T) {
	registered := []api.SSHKey{
		{ID: "a", Name: "alice", PublicKey: "ssh-ed25519 AAAATHEIRS alice@box"},
		{ID: "b", Name: "mine", PublicKey: "ssh-ed25519 AAAAMINE me@old-comment"},
	}

	// Matches on body even though the comment differs.
	if m, ok := matchRegisteredKey("ssh-ed25519 AAAAMINE me@new-laptop", registered); !ok || m.ID != "b" {
		t.Errorf("expected match on key b ignoring comment, got %+v ok=%v", m, ok)
	}
	// No body match → no reuse.
	if _, ok := matchRegisteredKey("ssh-ed25519 AAAAUNKNOWN me@laptop", registered); ok {
		t.Error("expected no match for an unregistered key body")
	}
	// Malformed local key never matches.
	if _, ok := matchRegisteredKey("garbage", registered); ok {
		t.Error("expected no match for a malformed key")
	}
}

func TestUpRequiresLogin(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := up(nil)
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in error, got: %v", err)
	}
}

func TestUpRejectsBothTemplates(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	// Seed a credential so the flag check is reached.
	_ = config.Save(&config.Credential{APIURL: "http://x", Token: "aq_sk", TeamID: "t"})
	err := up([]string{"--comfyui", "--jupyter"})
	if err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("expected mutually-exclusive error, got: %v", err)
	}
}
