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
