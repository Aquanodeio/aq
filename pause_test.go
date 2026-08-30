package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// Ticket #738: `aq pause` used to print `resume it any time with "aq up"`.
// `up()` accepts no setup argument at all — its only positional is
// `host:<alias>` — so it always rents a FRESH box, and everyone who followed
// that message landed on a new empty machine instead of their setup. The one
// command that can restore what pause just saved is
// `aq deploy --snapshot <deploymentId>`, because that id is what
// POST /deployments/deploy-snapshot takes as its `snapshotSource`.
func TestPausePrintsARestoreCommandThatCanTargetTheSetup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "11111111-2222-3333-4444-555555555555", "name": "trainer", "leaseDeploymentId": 4242},
		})
	})
	mux.HandleFunc("/deployments/4242", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": 4242, "project_id": "proj-1"})
	})
	paused := false
	mux.HandleFunc("/deployments/project/proj-1/pause", func(w http.ResponseWriter, r *http.Request) {
		paused = true
		writeData(w, map[string]any{"status": "PAUSED"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runPause(pauseOptions{cred: cred, target: "trainer", out: &out}); err != nil {
		t.Fatalf("runPause: %v", err)
	}
	if !paused {
		t.Fatal("pause route was never called")
	}

	got := out.String()
	if !strings.Contains(got, "aq deploy --snapshot 4242") {
		t.Errorf("pause did not print a usable restore command; got:\n%s", got)
	}
	if strings.Contains(got, "aq up") {
		t.Errorf("pause still points at `aq up`, which cannot target a setup; got:\n%s", got)
	}
}
