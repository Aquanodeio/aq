package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

func TestRunStatusReadyShowsURLAndCreds(t *testing.T) {
	mux := http.NewServeMux()
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
	var out bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, deploymentID: 4242, out: &out}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}

	got := out.String()
	for _, want := range []string{"ACTIVE", "https://comfy.box.aquanode.io", "admin", "s3cr3t"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q; got:\n%s", want, got)
		}
	}
	if gotAPIKey != "aq_sk_test" || gotTeamID != "team-1" {
		t.Errorf("auth headers not sent: key=%q team=%q", gotAPIKey, gotTeamID)
	}
}

func TestRunStatusStillProvisioning(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/7/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 7, "status": "PENDING",
			"deployment": map[string]any{"id": 7, "status": "PENDING"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runStatus(statusOptions{cred: cred, deploymentID: 7, out: &out}); err != nil {
		t.Fatalf("runStatus error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "PENDING") || !strings.Contains(got, "Still provisioning") {
		t.Errorf("expected still-provisioning output; got:\n%s", got)
	}
}

func TestStatusRequiresDeploymentID(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := status(nil)
	if err == nil || !strings.Contains(err.Error(), "deployment id is required") {
		t.Fatalf("expected missing-id error, got: %v", err)
	}
}

func TestStatusRejectsNonNumericID(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := status([]string{"abc"})
	if err == nil || !strings.Contains(err.Error(), "invalid deployment id") {
		t.Fatalf("expected invalid-id error, got: %v", err)
	}
}

func TestStatusRequiresLogin(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := status([]string{"4242"})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in error, got: %v", err)
	}
}
