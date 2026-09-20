package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

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

// imageCreateServer answers POST /jobs (and, when a test needs it, GET
// /jobs/hardware-availability for --any-gpu) for an image-source create,
// which never resolves a setup or version and so never hits /setups.
func imageCreateServer(t *testing.T, createJob http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", createJob)
	return httptest.NewServer(mux)
}

func baseImageCreateOpts(serverURL string) jobCreateOptions {
	return jobCreateOptions{
		cred:            &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL},
		image:           "docker.io/acme/train:latest",
		gpuModels:       []string{"H100", "A100"},
		diskGB:          200,
		maxInstances:    2,
		monthlyCapCents: -1,
		outputPath:      "/outputs",
		out:             &bytes.Buffer{},
	}
}

// decodeWireBody parses a raw request body into a generic map for
// order-independent structural comparison: the parity clause is about the
// JSON shape, not byte-for-byte text.
func decodeWireBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode wire body: %v (body: %s)", err, body)
	}
	return m
}

// TestCreateJobImageSourceMatchesConsoleBuildParams is the ticket's parity
// clause: the exact JSON an image-source `aq job create` sends must match
// what console/app/jobs/new/page.tsx's buildParams() produces for the same
// inputs (read directly off that function, not re-derived from this file's
// own code. Mocking the component whose contract is at risk would let a
// field-name break agree with itself).
//
// `checkpoint` is the one field in `wantJSON` NOT read off console's
// buildParams(): console's draft carries `{paths, intervalSeconds}`, a
// shape a console-literal transcription would get wrong for this field.
// The CLI's `checkpoint{paths,exclude}` is instead the shape the
// orchestrator's own `hasCheckpointPaths`/`checkpointRequired`
// (hardware.ts:67-105) actually validates, the field it runs against, not a
// sibling client's guess at it. TestCreateJobCheckpointAcceptedByTheRealOrchestrator
// below is what checks this against the live server itself.
//
// `wantJSON` deliberately excludes maxRuntimeSeconds, outputs, schedule,
// webhookUrl, secrets and the placement-target patch: buildParams()
// always/conditionally sends those too, but the ticket's own scope boundary
// says not to add --max-runtime-seconds/--schedule/--webhook-url/--outputs/
// --monthly-cap-cents beyond what already exists, so this CLI path never
// sends them and a full-body comparison would fail on an intentional,
// pre-existing gap rather than on this change.
func TestCreateJobImageSourceMatchesConsoleBuildParams(t *testing.T) {
	var body []byte
	srv := imageCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "job-1", "name": "train", "versionId": 0})
	})
	defer srv.Close()

	opts := baseImageCreateOpts(srv.URL)
	opts.registrySecret = "docker-hub"
	opts.gpuOrder = "ordered" // 2 models given, so console WOULD send this
	opts.argv = []string{"python", "train.py", "--epochs", "3"}
	opts.checkpointPaths = []string{"/workspace/checkpoints"}
	opts.checkpointExclude = []string{"/workspace/.venv"}
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}

	const wantJSON = `{
		"name": "train",
		"image": {"ref": "docker.io/acme/train:latest", "registrySecret": "docker-hub"},
		"maxInstances": 2,
		"hardware": {"gpuModels": ["H100", "A100"], "gpuCount": 1, "diskGb": 200},
		"placement": {"gpuOrder": "ordered"},
		"entrypoint": {"kind": "command", "argv": ["python", "train.py", "--epochs", "3"], "outputPath": "/outputs"},
		"checkpoint": {"paths": ["/workspace/checkpoints"], "exclude": ["/workspace/.venv"]}
	}`
	got := decodeWireBody(t, body)
	want := decodeWireBody(t, []byte(wantJSON))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire body does not match console buildParams() shape:\n got:  %s\nwant:  %s", body, wantJSON)
	}
}

// TestCreateJobCheckpointAbsentFromWireWhenNoFlagsGiven: neither
// --checkpoint-path nor --checkpoint-exclude was passed, so the `checkpoint`
// key must be ABSENT from the wire body entirely -- not `null`, not `{}` --
// letting the server's own checkpointRequired refusal name what is missing,
// rather than the CLI sending a present-but-empty key that reads differently.
func TestCreateJobCheckpointAbsentFromWireWhenNoFlagsGiven(t *testing.T) {
	var body []byte
	srv := imageCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "job-1", "name": "train"})
	})
	defer srv.Close()

	opts := baseImageCreateOpts(srv.URL)
	opts.argv = []string{"python", "train.py"}
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	if strings.Contains(string(body), "checkpoint") {
		t.Fatalf("checkpoint must be absent from the wire when neither flag is given, got: %s", body)
	}
}

// TestCreateJobCheckpointPresentOnWireWhenFlagsGiven asserts the wire carries
// exactly the given paths (and exclude list) once either flag is passed, on
// a VERSION-source create -- the ticket's fix applies to BOTH sources, since
// checkpointRequired runs unconditionally before any source branch.
func TestCreateJobCheckpointPresentOnWireWhenFlagsGiven(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.checkpointPaths = []string{"/workspace", "/data/out"}
	opts.checkpointExclude = []string{"/workspace/.venv", "/workspace/node_modules"}
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	var decoded api.CreateJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.Checkpoint == nil {
		t.Fatalf("checkpoint must be present on the wire once a flag is given, got: %s", body)
	}
	if !reflect.DeepEqual(decoded.Checkpoint.Paths, opts.checkpointPaths) {
		t.Errorf("checkpoint.paths = %v, want %v", decoded.Checkpoint.Paths, opts.checkpointPaths)
	}
	if !reflect.DeepEqual(decoded.Checkpoint.Exclude, opts.checkpointExclude) {
		t.Errorf("checkpoint.exclude = %v, want %v", decoded.Checkpoint.Exclude, opts.checkpointExclude)
	}
}

// TestCreateJobCheckpointAcceptedByTheRealOrchestrator is the ticket's
// "exercise the REAL create endpoint" clause. Every other test in this file
// asserts our own code against itself or against a literal an author typed
// -- exactly the shape of the bug being fixed here: aq#89's parity test
// agreed with a literal transcribed from reading console source while the
// real server required a field neither side carried. This test instead
// posts a real checkpoint-bearing create to a real orchestrator and asserts
// the SERVER accepts it.
//
// Gated behind AQ_LIVE_CREATE_TOKEN/AQ_LIVE_CREATE_TEAM_ID, deliberately not
// `config.Load()`: TestMain (main_test.go) sandboxes HOME/AQ_CONFIG_DIR for
// this whole package on every run, on purpose, so a test that forgot to
// isolate credentials or ssh config can never touch a developer's real
// setup -- config.Load() can therefore never see a real credential inside
// this package's tests no matter what env var toggles this test, and must
// not be routed around that. Reading two explicit env vars keeps `go test
// ./...` credential-free and prod-free by default (aq/CLAUDE.md's own
// standing gotcha is that `aq` reaches production by doing nothing), while
// still letting this test genuinely hit the real endpoint when asked. This
// never runs a Run and never rents a box (only a Run does), and it deletes
// the job it created in cleanup.
func TestCreateJobCheckpointAcceptedByTheRealOrchestrator(t *testing.T) {
	token := os.Getenv("AQ_LIVE_CREATE_TOKEN")
	teamID := os.Getenv("AQ_LIVE_CREATE_TEAM_ID")
	if token == "" || teamID == "" {
		t.Skip("set AQ_LIVE_CREATE_TOKEN and AQ_LIVE_CREATE_TEAM_ID to exercise the real orchestrator; skipped by default so go test ./... never touches prod")
	}
	apiURL := os.Getenv("AQ_LIVE_CREATE_API_URL")
	if apiURL == "" {
		apiURL = config.DefaultAPIURL
	}
	cred := &config.Credential{Token: token, TeamID: teamID, APIURL: apiURL}
	client := newControlClient(cred)

	avail, err := client.HardwareAvailability(20)
	if err != nil || len(avail.Models) == 0 {
		t.Fatalf("could not fetch a live GPU model to create against: %v", err)
	}

	name := fmt.Sprintf("aq-checkpoint-livecheck-%d", time.Now().UnixNano())
	opts := jobCreateOptions{
		cred:            cred,
		image:           "docker.io/library/alpine:3.20",
		gpuModels:       []string{avail.Models[0].GPUModel},
		diskGB:          20,
		maxInstances:    1,
		monthlyCapCents: -1,
		name:            name,
		argv:            []string{"/bin/sh", "-c", "echo hi > /outputs/r.txt"},
		outputPath:      "/outputs",
		checkpointPaths: []string{"/outputs"},
		out:             &bytes.Buffer{},
	}
	t.Cleanup(func() {
		jobs, err := client.ListJobs()
		if err != nil {
			return
		}
		for _, j := range jobs {
			if j.Name == name {
				_ = client.DeleteJob(j.ID)
			}
		}
	})

	if err := runJobCreate(opts); err != nil {
		t.Fatalf("the real orchestrator refused a checkpoint-bearing create: %v", err)
	}
}

// TestCreateJobImageSourceOmitsVersionIdFromTheWire: the SOURCE XOR is
// enforced on the WIRE, not just on the parsed Go struct: versionId must be
// ABSENT, never present as 0, or the backend's both-or-neither check
// (job.service.ts:618-632) reads the zero as "sent".
func TestCreateJobImageSourceOmitsVersionIdFromTheWire(t *testing.T) {
	var body []byte
	srv := imageCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "job-1", "name": "train"})
	})
	defer srv.Close()

	opts := baseImageCreateOpts(srv.URL)
	opts.argv = []string{"python", "train.py"}
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	if strings.Contains(string(body), "versionId") {
		t.Fatalf("an image-source create must never put versionId on the wire, got: %s", body)
	}
}

// TestCreateJobAnyGPUFetchesHardwareAvailabilityAndSendsEveryModel: --any-gpu
// is the explicit opt-in to the console's "no card picked means any card"
// default: it must fetch the model universe off GET
// /jobs/hardware-availability and send every one of those names, never an
// empty gpuModels list (placement refuses that outright).
func TestCreateJobAnyGPUFetchesHardwareAvailabilityAndSendsEveryModel(t *testing.T) {
	var body []byte
	availabilityHit := false
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs/hardware-availability", func(w http.ResponseWriter, r *http.Request) {
		availabilityHit = true
		if got := r.URL.Query().Get("diskGb"); got != "200" {
			t.Fatalf("hardware-availability diskGb = %q, want 200", got)
		}
		writeData(w, map[string]any{"models": []map[string]any{
			{"gpuModel": "H100"}, {"gpuModel": "RTX4090"},
		}, "offers": []any{}, "totalOffers": 0})
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "job-1", "name": "train"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	opts := baseImageCreateOpts(srv.URL)
	opts.gpuModels = nil
	opts.anyGPU = true
	opts.argv = []string{"python", "train.py"}
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}
	if !availabilityHit {
		t.Fatal("--any-gpu must fetch GET /jobs/hardware-availability")
	}
	var decoded api.CreateJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.Hardware == nil || !reflect.DeepEqual(decoded.Hardware.GPUModels, []string{"H100", "RTX4090"}) {
		t.Fatalf("hardware.gpuModels = %+v, want the full fetched universe [H100 RTX4090] (raw body: %s)", decoded.Hardware, body)
	}
}

// TestCreateJobVersionSourceCommandOverridesEntrypoint: a command (argv
// after `--`) applies to a VERSION-source create too, since
// deriveDefaultEntrypoint only ever returns non-null for ComfyUI, and every
// other template with no app port would otherwise 400 with no way to supply
// one. Supplying an entrypoint explicitly skips derivation, so the request
// must carry it in exactly the shape console/app/jobs/new/page.tsx's
// buildParams() sends (kind/argv/outputPath); hardware/placement stay
// UNSENT here, unlike the image-source path: this positional form never
// selects hardware from the CLI, the backend still seeds it from the
// version's own recipe.
func TestCreateJobVersionSourceCommandOverridesEntrypoint(t *testing.T) {
	var body []byte
	srv := jobCreateServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "myenv", "versionId": 555})
	})
	defer srv.Close()

	opts := baseCreateOpts(srv.URL)
	opts.argv = []string{"bash", "run.sh"}
	opts.outputPath = "/outputs"
	if err := runJobCreate(opts); err != nil {
		t.Fatalf("runJobCreate: %v", err)
	}

	got := decodeWireBody(t, body)
	entrypoint, _ := got["entrypoint"].(map[string]any)
	want := map[string]any{"kind": "command", "argv": []any{"bash", "run.sh"}, "outputPath": "/outputs"}
	if !reflect.DeepEqual(entrypoint, want) {
		t.Fatalf("entrypoint = %+v, want %+v (raw body: %s)", entrypoint, want, body)
	}
	if _, present := got["hardware"]; present {
		t.Fatalf("a version-source create must not send hardware, got: %s", body)
	}
}

// The five local refusals the ticket's Tests section names, all checked with
// no server running: each must be caught before requireLogin, let alone any
// network call.

// 1. No entrypoint on an image-source create.
func TestJobCreateImageWithoutEntrypointRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{"--image", "docker.io/acme/train:latest", "--gpu-model", "H100", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "must state its entrypoint") {
		t.Fatalf("error should say an entrypoint is required, got: %v", err)
	}
}

// 2. No --gpu-model (and no --any-gpu) on an image-source create.
func TestJobCreateImageWithoutGPUModelRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{"--image", "docker.io/acme/train:latest", "--max-instances", "1", "--", "python", "train.py"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--gpu-model is required") || !strings.Contains(err.Error(), "aq gpus") {
		t.Fatalf("error should require --gpu-model and name `aq gpus`, got: %v", err)
	}
}

// 3. --registry-secret without --image.
func TestJobCreateRegistrySecretWithoutImageRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1", "--registry-secret", "docker-hub"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--registry-secret") || !strings.Contains(err.Error(), "--image") {
		t.Fatalf("error should name --registry-secret and --image, got: %v", err)
	}
}

// 4. Both sources given at once.
func TestJobCreateBothSourcesRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{jobTestSetupID, "3", "--image", "docker.io/acme/train:latest", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "exactly one source") {
		t.Fatalf("error should say exactly one source is allowed, got: %v", err)
	}
}

// 5. Neither source given.
func TestJobCreateNeitherSourceRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{"--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage: aq job create") {
		t.Fatalf("error should print usage, got: %v", err)
	}
}

// --gpu-model and --any-gpu together are ambiguous, refused locally rather
// than silently preferring one.
func TestJobCreateGPUModelAndAnyGPURefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{
		"--image", "docker.io/acme/train:latest", "--gpu-model", "H100", "--any-gpu",
		"--max-instances", "1", "--", "python", "train.py",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error should say --gpu-model and --any-gpu are mutually exclusive, got: %v", err)
	}
}

// --on pins a box that already carries a saved version, so it cannot serve
// an --image job. Mirrors job.service.ts's own PinnedBoxError.
func TestJobCreateOnWithImageRefusesLocally(t *testing.T) {
	detachedSandbox(t, attachedHost())
	err := jobCreate([]string{
		"--image", "docker.io/acme/train:latest", "--gpu-model", "H100", "--on", "lease-a",
		"--max-instances", "1", "--", "python", "train.py",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cannot serve an --image job") {
		t.Fatalf("error should say a pinned box cannot serve an --image job, got: %v", err)
	}
}

// An argv token after `--` that itself looks like a flag (e.g. the user's own
// `--epochs 3`) must reach the entrypoint verbatim, never be re-parsed as an
// aq flag: the whole reason argv is split off before fs.Parse ever runs.
func TestJobCreateArgvLookingLikeAFlagIsNeverReparsed(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{
		jobTestSetupID, "3", "--max-instances", "1", "--", "python", "train.py", "--epochs", "3",
	})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("argv containing flag-shaped tokens should reach the login check untouched, got: %v", err)
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

// TestJobLogsWarnsOnceAboutADroppedTail: `truncated` is a LIVE, per-read flag
// on the box — once a log has rotated past its retained window it stays true
// for every remaining poll, not just the poll that crossed the rollover. An
// unguarded print therefore interleaved this warning into the user's log every
// two seconds for the rest of the run. Said once, like `unreachable` above it.
func TestJobLogsWarnsOnceAboutADroppedTail(t *testing.T) {
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
	})
	mux.HandleFunc("/jobs/job-1/runs/run-1/logs", func(w http.ResponseWriter, r *http.Request) {
		polls++
		// The box clamped the follower forward to the oldest retained byte,
		// so nextOffset is the SERVER's, never offset+len(chunk).
		writeData(w, map[string]any{
			"chunk":      fmt.Sprintf("line %d\n", polls),
			"offset":     1000 * polls,
			"nextOffset": 1000*polls + 7,
			"size":       9000,
			"truncated":  true,
			"source":     "live",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var out, errOut bytes.Buffer
	err := runJobLogs(jobLogsOptions{
		cred:     &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: srv.URL},
		jobRef:   "myjob",
		runID:    "run-1",
		follow:   true,
		maxPolls: 3,
		out:      &out,
		errOut:   &errOut,
		sleep:    func(time.Duration) {},
	})
	if err != nil {
		t.Fatalf("runJobLogs: %v", err)
	}
	if polls != 3 {
		t.Fatalf("polled %d times, want 3", polls)
	}
	if got := strings.Count(errOut.String(), "retained tail"); got != 1 {
		t.Fatalf("the dropped-tail warning was printed %d times across %d polls, want exactly 1:\n%s", got, polls, errOut.String())
	}
	if out.String() != "line 1\nline 2\nline 3\n" {
		t.Fatalf("the log itself must be unaffected, got %q", out.String())
	}
}
