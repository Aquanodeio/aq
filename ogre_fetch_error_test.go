package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// #967: the orchestrator answered 503 for every `aq import` (its task env was
// missing OGRE_ARTIFACT_REFRESH_SECRET) and the CLI printed "check your network
// connection". The user's network was never involved. This pins the copy at the
// call site, not just the classifier underneath it.
func TestEnsureOgreBinaryBlamesTheRightSide(t *testing.T) {
	// No ogre on PATH and a scratch install dir, so the fetch path is the one
	// under test rather than an ogre the developer happens to have installed.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	t.Run("server refusal names our side", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":false,"error":"MJOLNIR_BASE_URL and OGRE_ARTIFACT_REFRESH_SECRET must both be set"}`))
		}))
		defer srv.Close()

		_, err := ensureOgreBinary(api.New(srv.URL), &bytes.Buffer{})
		if err == nil {
			t.Fatal("expected an error from a 503")
		}
		// The copy may say "not your network"; what it must never do is send the
		// user off to check theirs.
		if strings.Contains(err.Error(), "check your network") {
			t.Fatalf("a 503 from our own API must not send the user to debug their network: %v", err)
		}
		if !strings.Contains(err.Error(), "our side") {
			t.Fatalf("a 503 from our own API must say it is ours: %v", err)
		}
		// The server's own reason must survive: it is the only clue an operator
		// reading a user's paste has.
		if !strings.Contains(err.Error(), "OGRE_ARTIFACT_REFRESH_SECRET") {
			t.Fatalf("the server's message must be preserved: %v", err)
		}
	})

	t.Run("unreachable server still mentions the network", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()

		_, err := ensureOgreBinary(api.New(url), &bytes.Buffer{})
		if err == nil {
			t.Fatal("expected a dial error")
		}
		if !strings.Contains(err.Error(), "network connection") {
			t.Fatalf("a genuine transport failure should still point at the network: %v", err)
		}
	})
}
