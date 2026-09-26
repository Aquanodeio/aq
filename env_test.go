package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/config"
)

// TestEnvDispatchesToKnownSubcommands checks `aq env` routes to the right
// subcommand and rejects an unknown one, matching `aq secret`'s dispatch
// shape.
func TestEnvDispatchesToKnownSubcommands(t *testing.T) {
	if err := env(nil); err == nil || !strings.Contains(err.Error(), "usage: aq env") {
		t.Errorf("expected a usage error with no args, got: %v", err)
	}
	if err := env([]string{"bogus"}); err == nil || !strings.Contains(err.Error(), `unknown subcommand "bogus"`) {
		t.Errorf("expected an unknown-subcommand error, got: %v", err)
	}
}

// TestRunEnvKeepPostsNameAndPrintsTheEnvironmentID checks `aq env keep`
// resolves the pod, posts {name}, and prints the returned environmentId.
func TestRunEnvKeepPostsNameAndPrintsTheEnvironmentID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/environment/keep", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"environmentId": "env-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runEnvKeep(envKeepOptions{cred: cred, target: "trainer", name: "pytorch-dev", out: &out}); err != nil {
		t.Fatalf("runEnvKeep: %v", err)
	}
	if !strings.Contains(out.String(), "env-1") || !strings.Contains(out.String(), "pytorch-dev") {
		t.Errorf("expected the kept name and id printed; got:\n%s", out.String())
	}
}

// TestRunEnvShareReadyImmediatelyPrintsTheLink checks a share that comes
// back already "ready" prints the link without polling.
func TestRunEnvShareReadyImmediatelyPrintsTheLink(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/environment/preview", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"includedRoots": []string{"/opt/app"}, "excluded": []string{}, "startupScript": "", "secretHits": []any{}})
	})
	var pollCalled bool
	mux.HandleFunc("/setups/pod-1/environment/share", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"shareId": "share-1", "shareUrl": "https://console.aquanode.io/launch/tok", "state": "ready"})
	})
	mux.HandleFunc("/shares/share-1", func(w http.ResponseWriter, r *http.Request) {
		pollCalled = true
		writeData(w, map[string]any{"state": "ready", "error": nil})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runEnvShare(envShareOptions{cred: cred, target: "trainer", out: &out}); err != nil {
		t.Fatalf("runEnvShare: %v", err)
	}
	if pollCalled {
		t.Error("must not poll /shares/:id when the share response already says ready")
	}
	if strings.TrimSpace(out.String()) != "Going out:\n  /opt/app\nhttps://console.aquanode.io/launch/tok" {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}

// TestRunEnvSharePollsUntilReady checks a "preparing" share polls
// GET /shares/:id until it flips to "ready" before printing the link.
func TestRunEnvSharePollsUntilReady(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/environment/preview", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"includedRoots": []string{}, "excluded": []string{}, "startupScript": "", "secretHits": []any{}})
	})
	mux.HandleFunc("/setups/pod-1/environment/share", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"shareId": "share-1", "shareUrl": "https://console.aquanode.io/launch/tok", "state": "preparing"})
	})
	polls := 0
	mux.HandleFunc("/shares/share-1", func(w http.ResponseWriter, r *http.Request) {
		polls++
		state := "preparing"
		if polls >= 2 {
			state = "ready"
		}
		writeData(w, map[string]any{"state": state, "error": nil})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	err := runEnvShare(envShareOptions{cred: cred, target: "trainer", out: &out, pollInterval: time.Millisecond, timeout: time.Second})
	if err != nil {
		t.Fatalf("runEnvShare: %v", err)
	}
	if polls < 2 {
		t.Errorf("expected at least 2 polls before ready, got %d", polls)
	}
	if !strings.Contains(out.String(), "Preparing link") || !strings.Contains(out.String(), "https://console.aquanode.io/launch/tok") {
		t.Errorf("expected a preparing notice then the link; got:\n%s", out.String())
	}
}

// TestRunEnvShareSurfacesAFailedPublish checks a share that ends "failed"
// surfaces the server's reason rather than a bare timeout-shaped error.
func TestRunEnvShareSurfacesAFailedPublish(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/environment/preview", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"includedRoots": []string{}, "excluded": []string{}, "startupScript": "", "secretHits": []any{}})
	})
	mux.HandleFunc("/setups/pod-1/environment/share", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"shareId": "share-1", "shareUrl": "", "state": "preparing"})
	})
	mux.HandleFunc("/shares/share-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"state": "failed", "error": "restic copy failed: repo locked"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runEnvShare(envShareOptions{cred: cred, target: "trainer", out: &bytes.Buffer{}, pollInterval: time.Millisecond, timeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "repo locked") {
		t.Fatalf("expected the server's failure reason surfaced, got: %v", err)
	}
}

// TestEnvShareRequiresVersionWithFromEnvironment checks the flag combination
// rules: --from-environment needs --version (an environment has no single
// pod to default the latest from), and a pod positional plus
// --from-environment together are refused as ambiguous.
func TestEnvShareRequiresVersionWithFromEnvironment(t *testing.T) {
	if err := envShare([]string{"--from-environment", "pytorch-dev"}); err == nil || !strings.Contains(err.Error(), "--version is required") {
		t.Errorf("expected a --version-required error, got: %v", err)
	}
	if err := envShare([]string{"trainer", "--from-environment", "pytorch-dev", "--version", "v1"}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected an ambiguous-target error, got: %v", err)
	}
}

// TestRunEnvLsWithNoTargetPrintsThreeGroups checks a bare `aq env ls`
// prints Built-in/Yours/Shared with you as three distinct sections.
func TestRunEnvLsWithNoTargetPrintsThreeGroups(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"builtin": []map[string]any{{"id": "b1", "name": "comfyui"}},
			"yours":   []map[string]any{{"id": "y1", "name": "pytorch-dev"}},
			"shared":  []map[string]any{},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runEnvLs(envLsOptions{cred: cred, out: &out}); err != nil {
		t.Fatalf("runEnvLs: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Built-in:", "comfyui", "Yours:", "pytorch-dev", "Shared with you:", "(none)"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q; got:\n%s", want, got)
		}
	}
}

// TestRunEnvLsWithPodTargetListsThatPodsVersions checks `aq env ls <pod>`
// resolves the pod first and lists ITS environment lineage, not the global
// picker.
func TestRunEnvLsWithPodTargetListsThatPodsVersions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/environment/versions", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "v2", "version": 2, "createdAt": "2026-09-20T00:00:00Z", "includedRoots": []string{"/opt"}, "excluded": []string{".ssh"}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runEnvLs(envLsOptions{cred: cred, target: "trainer", out: &out}); err != nil {
		t.Fatalf("runEnvLs: %v", err)
	}
	if !strings.Contains(out.String(), "v2") {
		t.Errorf("expected the pod's own version listed; got:\n%s", out.String())
	}
}

// TestRunEnvLsFallsBackToEnvironmentWhenNoPodMatches checks a target that
// doesn't match any pod falls back to resolving it as a named environment —
// read-only, so a wrong first guess costs nothing.
func TestRunEnvLsFallsBackToEnvironmentWhenNoPodMatches(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{}) // no pods at all
	})
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"builtin": []map[string]any{},
			"yours":   []map[string]any{{"id": "env-1", "name": "pytorch-dev"}},
			"shared":  []map[string]any{},
		})
	})
	mux.HandleFunc("/environments/env-1/versions", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "v1", "version": 1, "createdAt": "2026-09-01T00:00:00Z", "includedRoots": []string{}, "excluded": []string{}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runEnvLs(envLsOptions{cred: cred, target: "pytorch-dev", out: &out}); err != nil {
		t.Fatalf("runEnvLs: %v", err)
	}
	if !strings.Contains(out.String(), "v1") {
		t.Errorf("expected the environment's own version listed; got:\n%s", out.String())
	}
}

// TestRunEnvRmDeletesByResolvedID checks `aq env rm` resolves a name to an
// environment id and sends DELETE for that id.
func TestRunEnvRmDeletesByResolvedID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/environments", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"builtin": []map[string]any{},
			"yours":   []map[string]any{{"id": "env-1", "name": "pytorch-dev"}},
			"shared":  []map[string]any{},
		})
	})
	var gotMethod, gotPath string
	mux.HandleFunc("/environments/env-1", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeData(w, nil)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	if err := runEnvRm(envRmOptions{cred: cred, target: "pytorch-dev", out: &bytes.Buffer{}}); err != nil {
		t.Fatalf("runEnvRm: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/environments/env-1" {
		t.Errorf("%s %s, want DELETE /environments/env-1", gotMethod, gotPath)
	}
}
