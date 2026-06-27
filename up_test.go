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

// alwaysReady is a probe stub for tests that exercise the URL-publishing path
// without a real app to GET: it reports the published URL as serving immediately
// so the wait reaches printReady. Tests that specifically exercise the readiness
// probe (#234) inject their own stub instead.
func alwaysReady(string) bool { return true }

// writeError writes the orchestrator's `{success:false,error}` envelope with the
// given HTTP status, so tests can simulate a hard 4xx or a transient 5xx.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": msg})
}

// TestRunUpAbortsOnPermanentStatusError is the #208 regression: a permanent hard
// 4xx from the status endpoint must abort the wait fast with a diagnostic, not
// spin until the timeout.
func TestRunUpAbortsOnPermanentStatusError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")

	mux := http.NewServeMux()
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}})
	})
	mux.HandleFunc("/deployments/up", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 9, "projectId": "p", "status": "PENDING"})
	})
	mux.HandleFunc("/deployments/9/status", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		out:          &bytes.Buffer{},
		pollInterval: 2 * time.Millisecond,
		// A long timeout proves we abort on the 4xx rather than waiting it out.
		timeout: time.Hour,
	})
	if err == nil || !strings.Contains(err.Error(), "could not check deployment 9 status") {
		t.Fatalf("expected fast abort on permanent 4xx, got: %v", err)
	}
}

// TestRunUpKeepsPollingThroughTransient5xx verifies a transient 5xx hiccup does
// not kill the wait — polling continues until the service URL appears.
func TestRunUpKeepsPollingThroughTransient5xx(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")

	var polls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}})
	})
	mux.HandleFunc("/deployments/up", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 10, "projectId": "p", "status": "PENDING"})
	})
	mux.HandleFunc("/deployments/10/status", func(w http.ResponseWriter, r *http.Request) {
		// First poll hiccups with a 503; the next one comes up live.
		if atomic.AddInt32(&polls, 1) == 1 {
			writeError(w, http.StatusServiceUnavailable, "temporarily unavailable")
			return
		}
		writeData(w, map[string]any{"deploymentId": 10, "status": "ACTIVE", "deployment": map[string]any{
			"id": 10, "status": "ACTIVE", "service_credentials": map[string]any{
				"template": "comfyui", "url": "https://comfy.box.aquanode.io", "status": "running",
			},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		out:          &out,
		probe:        alwaysReady,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("transient 5xx should not abort the wait, got: %v", err)
	}
	if !strings.Contains(out.String(), "https://comfy.box.aquanode.io") {
		t.Errorf("expected the service URL after recovering from a 5xx; got:\n%s", out.String())
	}
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

	var out, errOut bytes.Buffer
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		gpuModel:     "RTX 4090",
		out:          &out,
		errOut:       &errOut,
		probe:        alwaysReady,
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
	if !strings.Contains(got, "admin") {
		t.Errorf("output missing username; got:\n%s", got)
	}
	// The password must NOT be echoed to stdout by default — it would land in
	// scrollback / CI logs / tee files (ticket #204). stderr gets a pointer.
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("password leaked into stdout; got:\n%s", got)
	}
	if !strings.Contains(errOut.String(), "--show-secrets") {
		t.Errorf("stderr missing the --show-secrets pointer; got:\n%s", errOut.String())
	}
}

// TestRunUpWaitsForAppPortBeforeReady is the #234 regression: the deployment can
// be ACTIVE with a published service URL before the app inside the box has bound
// its port. `aq up` must NOT print the URL as live until an HTTP probe to it
// succeeds — otherwise the user clicks a URL that connection-refuses.
func TestRunUpWaitsForAppPortBeforeReady(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@thismachine")

	server := &upServer{
		keys:          []map[string]any{{"id": "key-existing", "name": "laptop", "public_key": "ssh-ed25519 AAAA laptop"}},
		statusReadyAt: 1, // URL is published from the very first poll...
	}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	// ...but the app isn't serving yet: the probe fails twice, then succeeds.
	var probes int32
	notReadyUntil := int32(3)
	probe := func(u string) bool {
		if u != "https://comfy.box.aquanode.io" {
			t.Errorf("probe got unexpected URL %q", u)
		}
		return atomic.AddInt32(&probes, 1) >= notReadyUntil
	}

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		out:          &out,
		errOut:       &errOut,
		probe:        probe,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
	})
	if err != nil {
		t.Fatalf("runUp error: %v", err)
	}
	if got := atomic.LoadInt32(&probes); got < notReadyUntil {
		t.Errorf("expected the URL to be probed until it answered (>=%d), got %d", notReadyUntil, got)
	}
	gotOut := out.String()
	// While the app wasn't serving, the user sees a "waiting" line, not "is live".
	if !strings.Contains(gotOut, "waiting for ComfyUI to start serving") {
		t.Errorf("expected a 'waiting for the app to serve' message; got:\n%s", gotOut)
	}
	// "is live" must come only after the probe finally succeeded.
	if !strings.Contains(gotOut, "is live") || !strings.Contains(gotOut, "https://comfy.box.aquanode.io") {
		t.Errorf("expected the live URL once the probe succeeded; got:\n%s", gotOut)
	}
}

// TestRunUpTimesOutWhenAppNeverServes: the URL is published but the app never
// binds its port, so the probe never succeeds — the wait must time out rather
// than falsely report the URL live (#234).
func TestRunUpTimesOutWhenAppNeverServes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@thismachine")

	server := &upServer{
		keys:          []map[string]any{{"id": "key-existing", "name": "laptop", "public_key": "ssh-ed25519 AAAA laptop"}},
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
		probe:        func(string) bool { return false }, // app never serves
		pollInterval: 2 * time.Millisecond,
		timeout:      30 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout when the app never serves, got: %v", err)
	}
	if strings.Contains(out.String(), "is live") {
		t.Errorf("must not report the URL live when the probe never succeeds; got:\n%s", out.String())
	}
}

func TestRunUpShowSecretsEchoesPassword(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// The local key matches the registered one (comment differs) so ensureSSHKey
	// reuses it instead of failing on a missing ~/.ssh key in a clean CI HOME.
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@thismachine")

	srv := httptest.NewServer((&upServer{
		keys:          []map[string]any{{"id": "key-existing", "name": "laptop", "public_key": "ssh-ed25519 AAAA laptop"}},
		statusReadyAt: 1,
	}).handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}

	var out, errOut bytes.Buffer
	err := runUp(upOptions{
		cred:         cred,
		template:     templateComfyUI,
		showSecrets:  true,
		out:          &out,
		errOut:       &errOut,
		probe:        alwaysReady,
		pollInterval: 2 * time.Millisecond,
		timeout:      5 * time.Second,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("runUp error: %v", err)
	}
	if !strings.Contains(out.String(), "s3cr3t") {
		t.Errorf("--show-secrets should echo the password to stdout; got:\n%s", out.String())
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
		probe:        alwaysReady,
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
		probe:        alwaysReady,
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
