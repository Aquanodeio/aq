package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func TestParseInterspersedFlagAfterPositional(t *testing.T) {
	// `aq status 4242 --show-secrets` — flag after the positional id. The stdlib
	// flag package stops at the first positional, so parseInterspersed must keep
	// going to pick up the trailing flag (ticket #204).
	cases := [][]string{
		{"4242", "--show-secrets"},
		{"--show-secrets", "4242"},
	}
	for _, args := range cases {
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		show := fs.Bool("show-secrets", false, "")
		positional, err := parseInterspersed(fs, args)
		if err != nil {
			t.Fatalf("parseInterspersed(%v): %v", args, err)
		}
		if !*show {
			t.Errorf("parseInterspersed(%v): --show-secrets not parsed", args)
		}
		if len(positional) != 1 || positional[0] != "4242" {
			t.Errorf("parseInterspersed(%v): positional = %v, want [4242]", args, positional)
		}
	}
}

func TestRunDownRequestsClose(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	var gotBody map[string]any
	var gotAPIKey, gotTeamID string
	mux.HandleFunc("/deployments/close", func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotTeamID = r.Header.Get("x-team-id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeData(w, map[string]any{"status": "CLOSING"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runDown(downOptions{cred: cred, target: "4242", out: &out}); err != nil {
		t.Fatalf("runDown error: %v", err)
	}

	// The close body carries the deployment id as a number.
	if id, ok := gotBody["deploymentId"].(float64); !ok || int(id) != 4242 {
		t.Errorf("close body missing/wrong deploymentId: %#v", gotBody)
	}
	if gotAPIKey != "aq_sk_test" || gotTeamID != "team-1" {
		t.Errorf("auth headers not sent: key=%q team=%q", gotAPIKey, gotTeamID)
	}
	if !strings.Contains(out.String(), "Termination requested") {
		t.Errorf("expected termination confirmation; got:\n%s", out.String())
	}
}

func TestRunDownSurfacesAPIError(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/close", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "Deployment not found"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runDown(downOptions{cred: cred, target: "999", out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "Deployment not found") {
		t.Fatalf("expected not-found error, got: %v", err)
	}
}

func TestDownRequiresDeploymentID(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := down(nil)
	if err == nil || !strings.Contains(err.Error(), "deployment id is required") {
		t.Fatalf("expected missing-id error, got: %v", err)
	}
}

func TestDownRequiresLogin(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := down([]string{"4242"})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in error, got: %v", err)
	}
}

// A bare `aq down` calls the plain close route, which writes close_reason
// USER_REQUEST — excluded from RESUMABLE_CLOSE_REASONS, i.e. gone for good.
// This once printed only "termination requested", so the destructive path
// read no differently than a saved one. The disclosure must be printed
// BEFORE the terminate call, because afterwards the box is already gone.
// There is no more --save flag here (see stop.go: a pod's Stop is always
// save-then-release under the pod/environment/volume model). `aq down`
// stays the lower-level, always-unsaved "kill this deployment id" verb.
func TestRunDownDisclosesNothingIsSavedBeforeTerminating(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/close", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"status": "CLOSING"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runDown(downOptions{cred: cred, target: "4242", out: &out}); err != nil {
		t.Fatalf("runDown: %v", err)
	}

	got := out.String()
	beforeIdx := strings.Index(got, "without saving")
	terminateIdx := strings.Index(got, "Termination requested")
	if beforeIdx == -1 || terminateIdx == -1 || beforeIdx > terminateIdx {
		t.Errorf("expected the no-save disclosure BEFORE the termination line; got:\n%s", got)
	}
	if !strings.Contains(got, "cannot be resumed") {
		t.Errorf("expected the disclosure to say the box cannot be resumed; got:\n%s", got)
	}
}

// TestResolveAPIURLLetsTheEnvOverrideWin is a safety test, not a preference
// test. `aq login` persists the URL it paired against, so before this every
// logged-in user — everyone — kept talking to production no matter what
// AQ_API_URL said. Pointing it at a local stack LOOKED like it worked and
// quietly rented a real billable box in prod instead, which is exactly how a
// validation run leaked two live deployments.
func TestResolveAPIURLLetsTheEnvOverrideWin(t *testing.T) {
	stored := &config.Credential{APIURL: "https://api.aquanode.io/api/v1"}

	t.Setenv("AQ_API_URL", "http://localhost:11080/api/v1")
	if got := resolveAPIURL(stored); got != "http://localhost:11080/api/v1" {
		t.Fatalf("the env override must beat the stored credential, got %q", got)
	}

	t.Setenv("AQ_API_URL", "")
	if got := resolveAPIURL(stored); got != "https://api.aquanode.io/api/v1" {
		t.Fatalf("with no override the stored credential wins, got %q", got)
	}
	if got := resolveAPIURL(&config.Credential{}); got != config.DefaultAPIURL {
		t.Fatalf("with neither, the built-in default, got %q", got)
	}
	if got := resolveAPIURL(nil); got != config.DefaultAPIURL {
		t.Fatalf("a nil credential must not panic, got %q", got)
	}
}

// TestResolveDeploymentByNameMatchesPodName is the #1379 regression: a pod's
// deployment carries a generated name, so `aq ssh <pod name>` must resolve via
// GET /setups' attachedDeploymentId. Row shapes are captured from prod
// (2026-11-14: deployment 4103 "supply-watch-fire" with setup_id 01501926-...,
// setup "A6000 box"), with the status set to ACTIVE and attachedDeploymentId
// filled in, since no live row existed at capture time.
func TestResolveDeploymentByNameMatchesPodName(t *testing.T) {
	const setupID = "01501926-b73d-4804-b4f6-ca33bd5f82ad"
	deployments := []map[string]any{
		{"id": 4103, "name": "supply-watch-fire", "status": "ACTIVE", "setup_id": setupID},
		{"id": 4104, "name": "A6000 box", "status": "ACTIVE", "setup_id": nil},
		{"id": 4105, "name": "unrelated", "status": "ACTIVE", "setup_id": nil},
		{"id": 4106, "name": "old-run", "status": "CLOSED", "setup_id": "closed-setup"},
	}
	setups := []map[string]any{
		{"id": setupID, "name": "A6000 box", "status": "ready", "attachedDeploymentId": 4103},
		{"id": "stopped-setup", "name": "stopped-pod", "status": "ready", "attachedDeploymentId": nil},
		{"id": "closed-setup", "name": "closed-pod", "status": "ready", "attachedDeploymentId": 4106},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments", func(w http.ResponseWriter, r *http.Request) { writeData(w, deployments) })
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) { writeData(w, setups) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := api.NewAuthed(srv.URL, "aq_sk_test", "team-1")

	// The pod name and a different deployment's own name both match: ambiguous.
	_, err := resolveDeploymentByName(client, "a6000 BOX")
	var amb *ambiguousNameError
	if !errors.As(err, &amb) || len(amb.matches) != 2 {
		t.Fatalf("want ambiguity of 2, got err=%v", err)
	}

	// Without the colliding deployment name, the pod name alone resolves.
	deployments = deployments[:1]
	if id, err := resolveDeploymentByName(client, "A6000 box"); err != nil || id != 4103 {
		t.Fatalf("pod name: got id=%d err=%v, want 4103", id, err)
	}
	// A stopped pod and a pod whose deployment is closed never resolve.
	for _, n := range []string{"stopped-pod", "closed-pod"} {
		if id, err := resolveDeploymentByName(client, n); err == nil {
			t.Errorf("%q resolved to %d, want no match", n, id)
		}
	}
	// Today's deployment-name match still works.
	if id, err := resolveDeploymentByName(client, "supply-watch-fire"); err != nil || id != 4103 {
		t.Fatalf("deployment name: got id=%d err=%v, want 4103", id, err)
	}
}
