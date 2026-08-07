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

// TestDownWithSnapshotAbortsTerminateWhenCheckpointFails is the safety property
// --snapshot exists for: a failed save must leave the box running, never
// terminated unsaved.
func TestDownWithSnapshotAbortsTerminateWhenCheckpointFails(t *testing.T) {
	closed := false
	err := downWithCheckpoint(
		downOptions{snapshot: true, out: &bytes.Buffer{}},
		func(snapshotOptions) (api.CreateSnapshotResult, error) {
			return api.CreateSnapshotResult{}, errors.New("ogre agent unreachable")
		},
		func(downOptions) error { closed = true; return nil },
	)
	if err == nil {
		t.Fatal("want error when checkpoint fails")
	}
	if closed {
		t.Fatal("terminate ran after a failed checkpoint — the box would be destroyed unsaved")
	}
}

func TestDownWithSnapshotTerminatesAfterSuccessfulCheckpoint(t *testing.T) {
	closed := false
	err := downWithCheckpoint(
		downOptions{snapshot: true, out: &bytes.Buffer{}},
		func(snapshotOptions) (api.CreateSnapshotResult, error) {
			return api.CreateSnapshotResult{SnapshotID: "snap_42"}, nil
		},
		func(downOptions) error { closed = true; return nil },
	)
	if err != nil || !closed {
		t.Fatalf("err=%v closed=%v; want nil/true", err, closed)
	}
}

func TestDownWithoutSnapshotSkipsCheckpoint(t *testing.T) {
	checkpointed := false
	_ = downWithCheckpoint(
		downOptions{snapshot: false, out: &bytes.Buffer{}},
		func(snapshotOptions) (api.CreateSnapshotResult, error) {
			checkpointed = true
			return api.CreateSnapshotResult{}, nil
		},
		func(downOptions) error { return nil },
	)
	if checkpointed {
		t.Fatal("checkpoint ran without --snapshot")
	}
}
