package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// endpointTestSetupID is a UUID so resolveSetupID resolves it locally
// (looksLikeUUID) without a GET /setups round trip, the tests below only
// care about what runEndpointCreate sends to POST /endpoints.
const endpointTestSetupID = "11111111-1111-1111-1111-111111111111"

// endpointCreateServer answers the two lookups runEndpointCreate needs before
// it can build the CreateEndpointRequest (findSetup, ListAllSetupVersions),
// and hands POST /endpoints to createEndpoint so a test can assert on
// exactly the body that reached the wire.
func endpointCreateServer(t *testing.T, createEndpoint http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": endpointTestSetupID, "name": "myenv"}})
	})
	mux.HandleFunc("/setups/versions", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": 555, "version": 3, "setup_id": endpointTestSetupID}})
	})
	mux.HandleFunc("/endpoints", createEndpoint)
	return httptest.NewServer(mux)
}

func baseCreateOpts(serverURL string) endpointCreateOptions {
	return endpointCreateOptions{
		cred:          &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL},
		setupTarget:   endpointTestSetupID,
		version:       3,
		maxInstances:  1,
		spendCapCents: 500,
		out:           &bytes.Buffer{},
	}
}

// The managed (non-pinned) path must never put a pinnedDeploymentId key on
// the wire at all, CreateEndpointRequest.PinnedDeploymentID carries
// `omitempty` for exactly this, and this test asserts the wire, not just
// the parsed request struct.
func TestCreateEndpointOmitsPinnedDeploymentIDWhenNotPinned(t *testing.T) {
	var body []byte
	srv := endpointCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	if err := runEndpointCreate(opts); err != nil {
		t.Fatalf("runEndpointCreate: %v", err)
	}
	if strings.Contains(string(body), "pinnedDeploymentId") {
		t.Fatalf("managed-path request body must omit pinnedDeploymentId entirely, got: %s", body)
	}
}

// The --on path must send the resolved attached deployment id verbatim, and
// only that id, never a derived or rounded value.
func TestCreateEndpointSendsThePinnedDeploymentIDOnTheWire(t *testing.T) {
	var body []byte
	srv := endpointCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.pinnedDeploymentID = 4242
	opts.onAlias = "lease-a"
	opts.spendCapCents = 0
	if err := runEndpointCreate(opts); err != nil {
		t.Fatalf("runEndpointCreate: %v", err)
	}

	var decoded api.CreateEndpointRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.PinnedDeploymentID != 4242 {
		t.Fatalf("pinnedDeploymentId = %d, want 4242 (raw body: %s)", decoded.PinnedDeploymentID, body)
	}
}

// A pin the backend refuses (400, message names the fix) must reach the user
// verbatim, never buried inside "could not create endpoint ...: <msg>".
func TestCreateEndpointSurfacesA400VerbatimWhenPinned(t *testing.T) {
	const backendMsg = "deployment 4242 is not attached, attach it first with `aq attach`"
	srv := endpointCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, backendMsg)
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.pinnedDeploymentID = 4242
	opts.onAlias = "lease-a"
	err := runEndpointCreate(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != backendMsg {
		t.Fatalf("error = %q, want the backend message verbatim: %q", err.Error(), backendMsg)
	}
}

// The managed path keeps today's wrapped-error behaviour, this pins that
// widening the pinned path's error handling did not change it.
func TestCreateEndpointWrapsA400WhenNotPinned(t *testing.T) {
	srv := endpointCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "name already in use")
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	err := runEndpointCreate(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "could not create endpoint") || !strings.Contains(err.Error(), "name already in use") {
		t.Fatalf("expected the managed-path wrap to survive, got: %v", err)
	}
}

// endpointCreate (the flag-parsing entry point) must refuse an unknown --on
// alias locally, before requireLogin or any network call, no server is
// started for this test at all.
func TestEndpointCreateOnUnknownAliasRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := endpointCreate([]string{endpointTestSetupID, "3", "--max-instances", "1", "--on", "ghost"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "aq host ls") {
		t.Fatalf("error should name `aq host ls`, got: %v", err)
	}
}

// A registered-but-never-attached alias must also be refused locally, naming
// `aq attach <alias>` as the fix.
func TestEndpointCreateOnUnattachedAliasRefusesLocally(t *testing.T) {
	detachedSandbox(t, testHost()) // registered via `aq host add`, never attached
	err := endpointCreate([]string{endpointTestSetupID, "3", "--max-instances", "1", "--on", "lease-a"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not attached") || !strings.Contains(err.Error(), "aq attach lease-a") {
		t.Fatalf("error should say the box is not attached and name `aq attach lease-a`, got: %v", err)
	}
}

// --on must not require --spend-cap-cents: a pinned box bills nothing, so a
// cap on it can never fire. This is checked by confirming the command gets
// past the spend-cap validation (the next thing it hits is the login check,
// since detachedSandbox leaves no stored credential) rather than failing on
// the cap.
func TestEndpointCreateOnDoesNotRequireSpendCap(t *testing.T) {
	detachedSandbox(t, attachedHost())
	err := endpointCreate([]string{endpointTestSetupID, "3", "--max-instances", "1", "--on", "lease-a"})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if strings.Contains(err.Error(), "spend-cap-cents") {
		t.Fatalf("--on must not require --spend-cap-cents, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected to fail at the login check (proving the cap check was skipped), got: %v", err)
	}
}

// The ordinary managed path is untouched: omitting --spend-cap-cents without
// --on must still be a hard local error, and must never reach the network.
func TestEndpointCreateWithoutOnStillRequiresSpendCap(t *testing.T) {
	detachedSandbox(t)
	err := endpointCreate([]string{endpointTestSetupID, "3", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--spend-cap-cents is required") {
		t.Fatalf("expected the unchanged managed-path refusal, got: %v", err)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}
