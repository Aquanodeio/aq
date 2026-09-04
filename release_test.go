package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func attachedHost() config.Host {
	h := testHost()
	h.DeploymentID = 4242
	h.PublicHost = "1.2.3.4"
	h.OgrePort = 8443
	h.AttachedAt = "2026-08-27T00:00:00Z"
	return h
}

// stubReleaseRunner stands in for ssh in every release test. Release now
// contacts the box (it stops the ogre daemon attach started), so a test without
// a stub would make a REAL ssh attempt to a fake host and spend its connect
// timeout doing it.
func stubReleaseRunner(out string, err error) remoteRunner {
	return func(config.Host, string) ([]byte, error) { return []byte(out), err }
}

// okReleaseRunner is a box that stopped cleanly.
func okReleaseRunner() remoteRunner { return stubReleaseRunner("release_ok=1\n", nil) }

// Release must hit the release route and nothing else. A close/terminate call
// here would reach a provider adapter for a lease we do not hold — which is the
// single reason this verb exists as its own word.
func TestReleaseCallsReleaseAndNeverClose(t *testing.T) {
	detachedSandbox(t, attachedHost())

	var called []string
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/4242/release", func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.URL.Path)
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("release must call no other route; got %s %s", r.Method, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out bytes.Buffer
	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &out, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    okReleaseRunner(),
	})
	if err != nil {
		t.Fatalf("runRelease: %v", err)
	}
	if len(called) != 1 {
		t.Fatalf("expected exactly one call, got %v", called)
	}

	text := out.String()
	for _, want := range []string{"keeps running", "no provider is contacted", "not a terminate"} {
		if !strings.Contains(text, want) {
			t.Errorf("release output is missing %q:\n%s", want, text)
		}
	}
	// The copy must never call this a terminate or a detach — both words are
	// already taken and both would describe something destructive that this is
	// not.
	for _, forbidden := range []string{"Terminate", "terminated", "tear down", "Detach"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("release copy must not say %q:\n%s", forbidden, text)
		}
	}
}

// A host left carrying a deployment id whose row is gone would send every later
// verb at a deployment that no longer exists.
func TestReleaseClearsTheAttachStamp(t *testing.T) {
	detachedSandbox(t, attachedHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	}))
	defer srv.Close()

	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, keep: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    okReleaseRunner(),
	})
	if err != nil {
		t.Fatalf("runRelease: %v", err)
	}

	hosts, err := config.LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("--keep-host should leave the box in the registry, got %d", len(hosts))
	}
	if hosts[0].Attached() || hosts[0].DeploymentID != 0 || hosts[0].AttachedAt != "" {
		t.Fatalf("the attach stamp survived a release: %+v", hosts[0])
	}
}

// Without --keep-host the box leaves the registry entirely, but the machine is
// still untouched — forgetting it is not doing anything to it.
func TestReleaseWithoutKeepHostForgetsTheBox(t *testing.T) {
	detachedSandbox(t, attachedHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	}))
	defer srv.Close()

	if err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    okReleaseRunner(),
	}); err != nil {
		t.Fatalf("runRelease: %v", err)
	}
	if hosts, _ := config.LoadHosts(); len(hosts) != 0 {
		t.Fatalf("expected the box to be forgotten, got %+v", hosts)
	}
}

// pendingOnlyHost is what `aq attach` leaves behind after AdoptExternal
// succeeds but everything after it fails (redeem, box configuration, or the
// reachability probe). Attached() is false: nothing on the box was ever
// confirmed reachable.
func pendingOnlyHost() config.Host {
	h := testHost()
	h.PendingDeploymentID = 4242
	return h
}

// A refused attach prints "release it with `aq release
// <alias>`". This is that exact command, run against exactly the state a
// refused attach leaves behind — a host with PendingDeploymentID set and
// Attached() false — and it must succeed and hit the same release route a
// normal attached release does. Before the fix this returned "lease-a is not
// attached — there is nothing to release", the CLI's own refusal being
// unfollowable.
func TestReleaseClearsARefusedAttachsPendingRow(t *testing.T) {
	detachedSandbox(t, pendingOnlyHost())

	var called []string
	mux := http.NewServeMux()
	mux.HandleFunc("/deployments/4242/release", func(w http.ResponseWriter, r *http.Request) {
		called = append(called, r.URL.Path)
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("release must call no other route; got %s %s", r.Method, r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    okReleaseRunner(),
	})
	if err != nil {
		t.Fatalf("runRelease on a pending-only host: %v", err)
	}
	if len(called) != 1 {
		t.Fatalf("expected exactly one release call, got %v", called)
	}

	hosts, err := config.LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Fatalf("expected the box forgotten by default, got %+v", hosts)
	}
}

func TestReleaseRefusesADetachedBox(t *testing.T) {
	detachedSandbox(t, testHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("must not call the API for a detached box: %s", r.URL.Path)
	}))
	defer srv.Close()

	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "k", "t"),
	})
	if err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("expected a refusal for a detached box, got %v", err)
	}
}

// The probe reason is the entire value of a failed attach — it is what tells
// the user which port to open. It must survive the error envelope verbatim.
func TestActivateExternalSurfacesTheProbeReasonVerbatim(t *testing.T) {
	const reason = "dial tcp 203.0.113.7:8443: connect: connection refused"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"error":"unreachable","data":{"error":"unreachable","reason":"` + reason + `"}}`))
	}))
	defer srv.Close()

	_, err := api.NewAuthed(srv.URL, "k", "t").ActivateExternal(7)
	var unreachable *api.ExternalUnreachableError
	if err == nil || !errors.As(err, &unreachable) {
		t.Fatalf("expected an ExternalUnreachableError, got %v", err)
	}
	if unreachable.Reason != reason {
		t.Fatalf("reason = %q, want %q", unreachable.Reason, reason)
	}
}

// A release that could not reach the box is NOT a clean hand-back, and the exit
// code has to say so: a script reading a 0 must never conclude that a
// customer's machine carries none of our credentials.
func TestReleaseIsLoudWhenItCannotReachTheBox(t *testing.T) {
	detachedSandbox(t, attachedHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	}))
	defer srv.Close()

	var errOut bytes.Buffer
	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &errOut,
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    stubReleaseRunner("", errors.New("ssh: connect: connection refused")),
	})
	if err == nil {
		t.Fatal("an unreachable box must not report a clean release")
	}

	text := errOut.String()
	for _, want := range []string{"NOT fully cleaned", "could not stop the ogre daemon", "pkill -f"} {
		if !strings.Contains(text, want) {
			t.Errorf("the failure must say what is still on the box and how to finish; missing %q:\n%s", want, text)
		}
	}

	// The row is still gone. A machine we cannot reach must never be able to
	// hold the user's account hostage.
	hosts, err := config.LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Fatalf("the row must still be released, got %d hosts", len(hosts))
	}
}

// An ssh call that returned 0 is not a daemon that exited. Only the script's own
// marker proves that, so its absence is a failure even on a happy exit code.
func TestReleaseRefusesToCallASilentSshASuccess(t *testing.T) {
	detachedSandbox(t, attachedHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	}))
	defer srv.Close()

	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    stubReleaseRunner("some unrelated chatter\n", nil),
	})
	if err == nil {
		t.Fatal("a run with no release_ok marker must not read as a confirmed stop")
	}
}

// `boxCleared` absent is UNKNOWN: an orchestrator that predates the field
// answers exactly like one that could not reach the box, and neither may be
// optimistically read as done.
func TestReleaseTreatsAnAbsentBoxClearedAsUnknown(t *testing.T) {
	detachedSandbox(t, attachedHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"released": true})
	}))
	defer srv.Close()

	var errOut bytes.Buffer
	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &errOut,
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    okReleaseRunner(),
	})
	if err == nil {
		t.Fatal("an unconfirmed credential wipe must not read as a clean release")
	}
	if !strings.Contains(errOut.String(), "could not confirm it removed the credentials") {
		t.Errorf("unknown must be named as unknown:\n%s", errOut.String())
	}
}

// --force is the user accepting a box they know is dirty. It must still PRINT
// everything, and must never be the default.
func TestReleaseForceReportsRatherThanFails(t *testing.T) {
	detachedSandbox(t, attachedHost())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"released": true, "boxCleared": true})
	}))
	defer srv.Close()

	var errOut bytes.Buffer
	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, force: true, out: &bytes.Buffer{}, errOut: &errOut,
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run:    stubReleaseRunner("", errors.New("ssh: connect: connection refused")),
	})
	if err != nil {
		t.Fatalf("--force must accept a dirty box, got %v", err)
	}
	if !strings.Contains(errOut.String(), "NOT fully cleaned") {
		t.Errorf("--force must still report the state:\n%s", errOut.String())
	}
}

// A box that never finished attaching had nothing written to it, so there is
// nothing to reach and nothing to be unsure about, so the release stays quiet.
func TestReleaseOfANeverAttachedBoxDoesNotTouchTheMachine(t *testing.T) {
	h := testHost()
	h.PendingDeploymentID = 4242
	detachedSandbox(t, h)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"released": true})
	}))
	defer srv.Close()

	err := runRelease(releaseOptions{
		alias: "lease-a", yes: true, out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		client: api.NewAuthed(srv.URL, "aq_sk_test", "team-1"),
		run: func(config.Host, string) ([]byte, error) {
			t.Fatal("a never-attached box must not be contacted")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("runRelease: %v", err)
	}
}
