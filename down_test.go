package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

func TestRunDownRequestsClose(t *testing.T) {
	mux := http.NewServeMux()
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
	if err := runDown(downOptions{cred: cred, deploymentID: 4242, out: &out}); err != nil {
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
	mux.HandleFunc("/deployments/close", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "Deployment not found"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	err := runDown(downOptions{cred: cred, deploymentID: 999, out: &bytes.Buffer{}})
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
