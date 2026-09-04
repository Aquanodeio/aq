package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// A CLIENT TIMEOUT IS NOT A FAILED OPERATION.
//
// The activate handler used to hold this connection open for the whole of its
// post-attach box configuration, which ran a readiness poll to 303 seconds
// against a box that was answering the entire time. This CLI gives up after
// thirty, so a completed attach was reported as:
//
//	aq: could not activate deployment #NNNN: context deadline exceeded
//
// and the box was written off locally as never attached, which then made a
// later `aq release` describe an attached box as "never finished attaching".
// The user was told two operations had failed, and both had succeeded.
//
// The server no longer blocks like that, but the CLI must not depend on that
// being true: a timeout says nothing about what the server did, so the fix is
// to stop inferring an outcome we never observed and go and ask.

// impatient returns a client that times out faster than the stub server answers,
// which is the only way to exercise a real net/http timeout rather than a
// hand-rolled sentinel error that would prove nothing about the real path.
func impatient(baseURL string) *api.Client {
	c := api.NewAuthed(baseURL, "aq_sk_test", "team-1")
	c.HTTP.Timeout = 80 * time.Millisecond
	return c
}

// attachStubServer serves the attach handshake up to activate. `activate` sleeps
// past the client's patience; `status` answers with whatever the caller wants
// the row to say afterwards.
func attachStubServer(t *testing.T, statusCode int, status string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/external", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 4242, "installToken": "tok-1", "ogrePort": 8444})
	})
	mux.HandleFunc("/deployments/external/4242/install-config", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"ogreJwtSecret": "jwt-secret", "ogreProxyPassword": "proxy-pass",
			"tlsCertPem": "-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n",
			"tlsKeyPem":  "-----BEGIN PRIVATE KEY-----\nBBB\n-----END PRIVATE KEY-----\n",
			"ogrePort":   8444, "orchestratorUrl": "https://server.example", "deploymentId": 4242,
		})
	})
	// The shape that caused the bug: the server is working, it is just not
	// finished, and the client stops listening first.
	mux.HandleFunc("/deployments/external/4242/activate", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		writeData(w, map[string]any{"status": "ACTIVE"})
	})
	mux.HandleFunc("/deployments/4242/status", func(w http.ResponseWriter, r *http.Request) {
		if statusCode != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(statusCode)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "not found"})
			return
		}
		writeData(w, map[string]any{"deploymentId": 4242, "status": status})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func attachStubRunner() remoteRunner {
	return func(_ config.Host, remote string) ([]byte, error) {
		switch {
		case strings.Contains(remote, "preflight_ok"):
			return []byte(attachPreflightOutput("readable", customerKeys, "free")), nil
		case strings.Contains(remote, "__AQ_ABSENT__"):
			return []byte("__AQ_ABSENT__\n"), nil
		default:
			return []byte(""), nil
		}
	}
}

// THE REGRESSION. The row went ACTIVE, so the attach succeeded, so the command
// must succeed and the box must be recorded as attached.
func TestAttachReportsSuccessWhenTheRowWentActiveDespiteATimeout(t *testing.T) {
	detachedSandbox(t, testHost())
	srv := attachStubServer(t, http.StatusOK, "ACTIVE")

	var out bytes.Buffer
	err := runAttach(attachOptions{
		alias: "lease-a", yes: true, out: &out, errOut: &bytes.Buffer{},
		client: impatient(srv.URL),
		run:    attachStubRunner(),
	})
	if err != nil {
		t.Fatalf("an attach whose row is ACTIVE must not be reported as a failure: %v", err)
	}

	hosts, _ := config.LoadHosts()
	if !hosts[0].Attached() {
		t.Fatal("a box that attached must be recorded as attached, not left pending")
	}
	// The specific knock-on that made this worse than a cosmetic wrong message:
	// a leftover PendingDeploymentID is what made `aq release` announce an
	// attached box as one that "never finished attaching".
	if hosts[0].PendingDeploymentID != 0 {
		t.Errorf("a completed attach must clear PendingDeploymentID, got %d", hosts[0].PendingDeploymentID)
	}
	if !strings.Contains(out.String(), "is attached as deployment #4242") {
		t.Errorf("the user must be told the attach succeeded:\n%s", out.String())
	}
	// It saw an ACTIVE row and nothing else, so it must not claim a verdict on
	// the optional capabilities it never observed.
	if strings.Contains(out.String(), "Browser terminal: not available") {
		t.Errorf("a verdict this path never received must not be rendered as a failure:\n%s", out.String())
	}
}

// The other half of the same rule: a timeout on a row that did NOT go active is
// still a failure, and must stay one. Recovering from a timeout must never
// become "assume it worked".
func TestAttachStillFailsWhenTheRowDidNotGoActive(t *testing.T) {
	detachedSandbox(t, testHost())
	srv := attachStubServer(t, http.StatusOK, "PROVISIONING")

	var out bytes.Buffer
	err := runAttach(attachOptions{
		alias: "lease-a", yes: true, out: &out, errOut: &bytes.Buffer{},
		client: impatient(srv.URL),
		run:    attachStubRunner(),
	})
	if err == nil {
		t.Fatal("a timeout on a row that never activated must remain a failure")
	}
	hosts, _ := config.LoadHosts()
	if hosts[0].Attached() {
		t.Fatal("a box that did not activate must never be recorded as attached")
	}
	// It stays releasable, which is what lets the user clear the server-side row
	// the adopt call created.
	if hosts[0].PendingDeploymentID != 4242 {
		t.Errorf("an unfinished attach must stay releasable, got PendingDeploymentID %d", hosts[0].PendingDeploymentID)
	}
}

// "Could not look" is not a verdict either. If the follow-up question cannot be
// answered, the original timeout stands rather than being resolved either way.
func TestAttachKeepsTheTimeoutWhenItCannotAskWhatHappened(t *testing.T) {
	detachedSandbox(t, testHost())
	srv := attachStubServer(t, http.StatusInternalServerError, "")

	var out bytes.Buffer
	err := runAttach(attachOptions{
		alias: "lease-a", yes: true, out: &out, errOut: &bytes.Buffer{},
		client: impatient(srv.URL),
		run:    attachStubRunner(),
	})
	if err == nil {
		t.Fatal("an unanswerable follow-up must leave the timeout standing, not resolve it")
	}
	if hosts, _ := config.LoadHosts(); hosts[0].Attached() {
		t.Fatal("an unknown outcome must never be recorded as attached")
	}
}

func TestIsTimeoutSeparatesOurImpatienceFromTheServersAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		writeData(w, map[string]any{})
	}))
	defer srv.Close()

	_, err := impatient(srv.URL).ActivateExternal(1)
	if !api.IsTimeout(err) {
		t.Fatalf("a real client timeout must be recognized as one, got %v", err)
	}

	// A server that ANSWERED, with a refusal, is not a timeout: that is a
	// verdict and must be reported as the failure it is.
	refuser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false, "error": "unreachable",
			"data": map[string]any{"reason": "dial tcp: i/o timeout"},
		})
	}))
	defer refuser.Close()

	_, err = api.NewAuthed(refuser.URL, "k", "t").ActivateExternal(1)
	if err == nil {
		t.Fatal("a 409 must be an error")
	}
	// Note the server's own words here contain "i/o timeout". A substring match
	// on the message would call this a client timeout and silently convert a
	// real refusal into a "go and ask" recovery.
	if api.IsTimeout(err) {
		t.Errorf("a server refusal must never be mistaken for a client timeout: %v", err)
	}
}

// THE RELEASE HALF OF THE SAME FALSE NEGATIVE.
//
// `aq release` client-timed out while the server went on to complete the
// release and drop the row, so the user was told the release had failed on a
// box that had already been handed back, and was left holding a local host
// entry addressing a deployment that no longer exists.
//
// A release DELETES the row, so the follow-up question has a clean answer: a
// 404 is exactly what a successful release leaves behind.
func TestReleaseReportsSuccessWhenTheRowIsGoneDespiteATimeout(t *testing.T) {
	detachedSandbox(t, attachedHost())
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/4242/release", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	})
	mux.HandleFunc("/deployments/4242/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "not found"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out bytes.Buffer
	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, keep: true, out: &out, errOut: &bytes.Buffer{},
		client: impatient(srv.URL),
		run:    okReleaseRunner(),
	})
	// The release LANDED and the output says so. It still exits non-zero, and
	// that is the pre-existing three-state posture rather than a regression: aq
	// never saw the response, so it does not know whether the credentials it
	// pushed were removed from the customer's machine, and an unconfirmed wipe
	// is deliberately loud (--force accepts it). What changed is WHICH failure
	// this is. It used to be "could not release deployment #4242: context
	// deadline exceeded", which is false and leaves the user with a local entry
	// pointing at a row that no longer exists.
	if err == nil {
		t.Fatal("an unconfirmed wipe must stay loud")
	}
	if !strings.Contains(err.Error(), "released the row") {
		t.Errorf("the user must be told the release landed, not that it failed: %v", err)
	}
	if strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("a completed release must never be reported as a client timeout: %v", err)
	}
	hosts, _ := config.LoadHosts()
	if hosts[0].Attached() || hosts[0].PendingDeploymentID != 0 {
		t.Error("a completed release must clear every attach stamp")
	}
	if strings.Contains(out.String(), "credentials were removed from the box") {
		t.Errorf("an unobserved wipe must not be reported as clean:\n%s", out.String())
	}
}

// A row that is still THERE means the release did not land, and the timeout
// must stand as the failure it is.
func TestReleaseStillFailsWhenTheRowSurvives(t *testing.T) {
	detachedSandbox(t, attachedHost())
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/4242/release", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		writeData(w, map[string]any{"released": true})
	})
	mux.HandleFunc("/deployments/4242/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 4242, "status": "ACTIVE"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, keep: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: impatient(srv.URL),
		run:    okReleaseRunner(),
	})
	if err == nil {
		t.Fatal("a release whose row survives must remain a failure")
	}
	if hosts, _ := config.LoadHosts(); !hosts[0].Attached() {
		t.Error("a release that did not land must leave the box attached")
	}
}
