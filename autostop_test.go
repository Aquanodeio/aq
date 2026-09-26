package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// TestAutostopParsesOnOff checks `aq autostop <pod> on|off` parses the
// required two positionals and rejects anything else.
func TestAutostopParsesOnOff(t *testing.T) {
	if err := autostop([]string{"trainer"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected a usage error with one positional, got: %v", err)
	}
	if err := autostop([]string{"trainer", "maybe"}); err == nil || !strings.Contains(err.Error(), `"on" or "off"`) {
		t.Errorf("expected an on/off error, got: %v", err)
	}
}

// TestRunAutostopPutsToAutostopPathAndRendersThreeStates checks
// `aq autostop` hits PUT /setups/:id/autostop and never collapses a nil
// AutostopEnabled into "off".
func TestRunAutostopPutsToAutostopPathAndRendersThreeStates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	var gotMethod, gotPath string
	mux.HandleFunc("/setups/pod-1/autostop", func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		writeData(w, map[string]any{"id": "pod-1", "name": "trainer", "autostopEnabled": true})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runAutostop(autostopOptions{cred: cred, target: "trainer", enabled: true, out: &out}); err != nil {
		t.Fatalf("runAutostop: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/setups/pod-1/autostop" {
		t.Errorf("%s %s, want PUT /setups/pod-1/autostop", gotMethod, gotPath)
	}
	if !strings.Contains(out.String(), "Auto-stop is now on") {
		t.Errorf("expected the on state printed; got:\n%s", out.String())
	}
}

// TestRunAutostopRendersUnsetHonestly checks a nil AutostopEnabled in the
// response (a surprising server reply) is rendered as unset, never silently
// treated as off: the three-state signal rule.
func TestRunAutostopRendersUnsetHonestly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "pod-1", "name": "trainer"}})
	})
	mux.HandleFunc("/setups/pod-1/autostop", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": "pod-1", "name": "trainer", "autostopEnabled": nil})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runAutostop(autostopOptions{cred: cred, target: "trainer", enabled: false, out: &out}); err != nil {
		t.Fatalf("runAutostop: %v", err)
	}
	if !strings.Contains(out.String(), "unset") {
		t.Errorf("expected the unset state rendered honestly; got:\n%s", out.String())
	}
}
