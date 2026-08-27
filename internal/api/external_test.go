package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The orchestrator serialises `deploymentId` as the JSON NUMBER it is in
// Postgres — `Deployment.id` is an int, and `AdoptExternalResult` on this side
// already decodes it as one. `ExternalInstallConfig.DeploymentID` was typed
// `string`, and because nothing read the field the mistake survived unit tests,
// a build, a review and a merge: it only surfaced on the first live `aq attach`,
// where `json.Unmarshal` failed and attach could never complete.
//
// These tests decode the ACTUAL bytes the route emits rather than a Go value
// round-tripped through our own struct — a struct-to-struct test cannot catch a
// type disagreement with the other repo.
const installConfigWireBody = `{
  "success": true,
  "data": {
    "ogreJwtSecret": "s3cret",
    "ogreProxyPassword": "pr0xy",
    "tlsCertPem": "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n",
    "tlsKeyPem": "-----BEGIN PRIVATE KEY-----\nMIIE\n-----END PRIVATE KEY-----\n",
    "ogrePort": 8443,
    "orchestratorUrl": "https://server.aquanode.io",
    "deploymentId": 3390
  }
}`

func TestExternalInstallConfigDecodesNumericDeploymentId(t *testing.T) {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(installConfigWireBody), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	var cfg ExternalInstallConfig
	if err := json.Unmarshal(env.Data, &cfg); err != nil {
		t.Fatalf("install config must decode the orchestrator's numeric deploymentId: %v", err)
	}
	// Deliberately asserts only fields that predate the fix, so this test fails
	// on the ASSERTION (a decode error) rather than failing to compile when the
	// broken `string` typing is restored — a compile break is a weaker negative
	// control than a red test.
	if cfg.OgrePort != 8443 || cfg.OgreJWTSecret != "s3cret" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestRedeemExternalInstallConfigRejectsAnEchoForAnotherDeployment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
			t.Errorf("Authorization = %q, want the install token as Bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		// Same body, but naming a DIFFERENT deployment than the path asked for.
		_, _ = w.Write([]byte(strings.Replace(installConfigWireBody, "3390", "9999", 1)))
	}))
	defer srv.Close()

	c := NewAuthed(srv.URL, "should-not-be-sent", "team")
	if _, err := c.RedeemExternalInstallConfig(3390, "tok-123"); err == nil {
		t.Fatal("a config naming another deployment must be refused, not installed")
	} else if !strings.Contains(err.Error(), "9999") || !strings.Contains(err.Error(), "3390") {
		t.Fatalf("error must name both deployments, got: %v", err)
	}
}

func TestRedeemExternalInstallConfigAcceptsTheMatchingEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(installConfigWireBody))
	}))
	defer srv.Close()

	c := New(srv.URL)
	cfg, err := c.RedeemExternalInstallConfig(3390, "tok-123")
	if err != nil {
		t.Fatalf("matching echo must succeed: %v", err)
	}
	if cfg.DeploymentID != 3390 {
		t.Fatalf("deploymentId = %d, want 3390", cfg.DeploymentID)
	}
}
