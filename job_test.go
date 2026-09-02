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

// jobTestSetupID is a UUID so resolveSetupID resolves it locally
// (looksLikeUUID) without a GET /setups round trip, the tests below only
// care about what runJobCreate sends to POST /jobs.
const jobTestSetupID = "11111111-1111-1111-1111-111111111111"

// jobCreateServer answers the two lookups runJobCreate needs before
// it can build the CreateJobRequest (findSetup, ListAllSetupVersions),
// and hands POST /jobs to createJob so a test can assert on
// exactly the body that reached the wire.
func jobCreateServer(t *testing.T, createJob http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": jobTestSetupID, "name": "myenv"}})
	})
	mux.HandleFunc("/setups/versions", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": 555, "version": 3, "setup_id": jobTestSetupID}})
	})
	mux.HandleFunc("/jobs", createJob)
	return httptest.NewServer(mux)
}

func baseCreateOpts(serverURL string) jobCreateOptions {
	return jobCreateOptions{
		cred:         &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL},
		setupTarget:  jobTestSetupID,
		version:      3,
		maxInstances: 1,
		// -1 is "no monthly budget requested", which is the ordinary case now
		// that the per-job dollar cap is gone.
		monthlyCapCents: -1,
		out:             &bytes.Buffer{},
	}
}

// The managed (non-pinned) path must never put a pinnedDeploymentId key on
// the wire at all, CreateJobRequest.PinnedDeploymentID carries
// `omitempty` for exactly this, and this test asserts the wire, not just
// the parsed request struct.
func TestCreateJobOmitsPinnedDeploymentIDWhenNotPinned(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	if strings.Contains(string(body), "pinnedDeploymentId") {
		t.Fatalf("managed-path request body must omit pinnedDeploymentId entirely, got: %s", body)
	}
}

// The --on path must send the resolved attached deployment id verbatim, and
// only that id, never a derived or rounded value.
func TestCreateJobSendsThePinnedDeploymentIDOnTheWire(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.pinnedDeploymentID = 4242
	opts.onAlias = "lease-a"
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}

	var decoded api.CreateJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.PinnedDeploymentID != 4242 {
		t.Fatalf("pinnedDeploymentId = %d, want 4242 (raw body: %s)", decoded.PinnedDeploymentID, body)
	}
}

// A pin the backend refuses (400, message names the fix) must reach the user
// verbatim, never buried inside "could not create job ...: <msg>".
func TestCreateJobSurfacesA400VerbatimWhenPinned(t *testing.T) {
	const backendMsg = "deployment 4242 is not attached, attach it first with `aq attach`"
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, backendMsg)
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.pinnedDeploymentID = 4242
	opts.onAlias = "lease-a"
	err := runJobCreate(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != backendMsg {
		t.Fatalf("error = %q, want the backend message verbatim: %q", err.Error(), backendMsg)
	}
}

// The managed path keeps today's wrapped-error behaviour, this pins that
// widening the pinned path's error handling did not change it.
func TestCreateJobWrapsA400WhenNotPinned(t *testing.T) {
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "name already in use")
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	err := runJobCreate(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "could not create job") || !strings.Contains(err.Error(), "name already in use") {
		t.Fatalf("expected the managed-path wrap to survive, got: %v", err)
	}
}

// jobCreate (the flag-parsing entry point) must refuse an unknown --on
// alias locally, before requireLogin or any network run, no server is
// started for this test at all.
func TestJobCreateOnUnknownAliasRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1", "--on", "ghost"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "aq host ls") {
		t.Fatalf("error should name `aq host ls`, got: %v", err)
	}
}

// A registered-but-never-attached alias must also be refused locally, naming
// `aq attach <alias>` as the fix.
func TestJobCreateOnUnattachedAliasRefusesLocally(t *testing.T) {
	detachedSandbox(t, testHost()) // registered via `aq host add`, never attached
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1", "--on", "lease-a"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not attached") || !strings.Contains(err.Error(), "aq attach lease-a") {
		t.Fatalf("error should say the box is not attached and name `aq attach lease-a`, got: %v", err)
	}
}

// Creating a job needs NO cap flag at all now, on either path. The check is
// that the command gets past argument validation and fails at the login check
// instead (detachedSandbox leaves no stored credential), which proves nothing
// local refused it first.
func TestJobCreateNeedsNoCapFlag(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected to reach the login check with no cap flag, got: %v", err)
	}
}

// The same on the pinned path.
func TestJobCreateOnNeedsNoCapFlag(t *testing.T) {
	detachedSandbox(t, attachedHost())
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1", "--on", "lease-a"})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected to reach the login check, got: %v", err)
	}
}

// --spend-cap-cents is DELETED, and passing it must FAIL LOUDLY rather than be
// accepted and ignored.
//
// This is the point of the whole change. The backend deleted the per-job dollar
// cap, and a CLI that kept accepting the flag as a no-op would keep telling
// people they had a hard stop they no longer have, which is the exact
// complaint the cap was removed over. An unknown-flag error sends someone
// with an old script to read what replaced it; a silent no-op sends them to
// a surprise bill.
func TestJobCreateRejectsTheDeletedSpendCapFlag(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1", "--spend-cap-cents", "500"})
	if err == nil {
		t.Fatal("expected --spend-cap-cents to be rejected, not silently accepted")
	}
	if !strings.Contains(err.Error(), "spend-cap-cents") {
		t.Fatalf("the error should name the flag that no longer exists, got: %v", err)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// The monthly budget is OPTIONAL, so an unset one must be ABSENT from the
// request body — not present as 0.
//
// This asserts the WIRE, not the parsed struct. A zero on the wire would be
// read by the backend as a budget of nothing and refuse every run of the job,
// and a parsed-value check ("is it zero?") cannot tell an omitted key from a
// sent zero, so it would pass while every real create was broken.
func TestCreateJobOmitsMonthlyCapWhenUnset(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL) // monthlyCapCents: -1
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	if strings.Contains(string(body), "monthlySpendCapCents") {
		t.Fatalf("an unset monthly budget must not appear on the wire at all, got: %s", body)
	}
}

// And a budget that WAS set is sent verbatim, including a deliberate 0 — which
// is a real choice ("stop after this month's first cent") and distinct from
// "no budget".
func TestCreateJobSendsMonthlyCapWhenSet(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.monthlyCapCents = 2500
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	if !strings.Contains(string(body), `"monthlySpendCapCents":2500`) {
		t.Fatalf("monthly budget missing from the wire, got: %s", body)
	}
}

// jobRunFollowServer answers the three calls a `--follow` run needs: the job
// list (resolveJobID, hit once by doJobRun and again by runJobLogs since the
// two share no client instance), POST /jobs/:id/runs to start it, and the
// log tail. The log source is "archived" on the very first poll so the
// follow loop returns immediately without sleeping, this test is about
// wiring the two together, not the poll timing runJobLogs already covers.
func jobRunFollowServer(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	logsHit := false
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/jobs/job-1/runs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"runId":      "run-1",
			"acceptedAt": "2026-09-02T00:00:00Z",
			"status":     "queued",
		})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs", func(w http.ResponseWriter, r *http.Request) {
		logsHit = true
		writeData(w, map[string]any{
			"chunk":      "hello from the box\n",
			"nextOffset": 20,
			"size":       20,
			"truncated":  false,
			"source":     "archived",
		})
	})
	return httptest.NewServer(mux), &logsHit
}

// TestDoJobRunFollowStreamsTheLogAfterCreating: `--follow` is create-then-
// tail, sharing runJobLogs rather than a second poll loop. This asserts both
// halves actually ran: the run got created AND its log got read, in that
// order, through the one shared implementation.
func TestDoJobRunFollowStreamsTheLogAfterCreating(t *testing.T) {
	srv, logsHit := jobRunFollowServer(t)
	defer srv.Close()

	var out bytes.Buffer
	var errOut bytes.Buffer
	opts := jobRunOptions{
		cred:   &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: srv.URL},
		target: "myjob",
		inputs: map[string]any{},
		follow: true,
		out:    &out,
		errOut: &errOut,
	}
	if err := doJobRun(opts); err != nil {
		t.Fatalf("doJobRun: %v", err)
	}
	if !*logsHit {
		t.Fatal("--follow must read the run's log, not just create it")
	}
	if !strings.Contains(out.String(), "run-1") {
		t.Fatalf("want the created run id announced before following, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "hello from the box") {
		t.Fatalf("want the tailed log chunk printed, got: %s", out.String())
	}
}

// TestDoJobRunWithoutFollowNeverTailsTheLog: the default `aq job run`
// behaviour (create, maybe wait, print, return) must not change just because
// runJobLogs is now reachable from this file.
func TestDoJobRunWithoutFollowNeverTailsTheLog(t *testing.T) {
	srv, logsHit := jobRunFollowServer(t)
	defer srv.Close()

	var out bytes.Buffer
	opts := jobRunOptions{
		cred:   &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: srv.URL},
		target: "myjob",
		inputs: map[string]any{},
		out:    &out,
	}
	if err := doJobRun(opts); err != nil {
		t.Fatalf("doJobRun: %v", err)
	}
	if *logsHit {
		t.Fatal("without --follow the log endpoint must never be hit")
	}
}
