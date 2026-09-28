package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// jobTestSetupID is a UUID so resolveSetupID resolves it locally
// (looksLikeUUID) without a GET /setups round trip, the tests below only
// care about what runJobRun sends to POST /jobs.
const jobTestSetupID = "11111111-1111-1111-1111-111111111111"

// jobRunServer answers the two lookups runJobRun needs before it can build
// the CreateBatchJobRequest (findSetup, ListAllSetupVersions), and hands
// POST /jobs to createJob so a test can assert on exactly the body that
// reached the wire.
func jobRunServer(t *testing.T, createJob http.HandlerFunc) *httptest.Server {
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

// baseRunOpts is a version-source run with detach:true — every test here
// that only cares about the wire body or a local refusal skips the log
// stream and status poll entirely; streaming itself is covered separately by
// TestRunJobRunStreamsToCompletion* below.
func baseRunOpts(serverURL string) jobRunOptions {
	return jobRunOptions{
		cred:        &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL},
		setupTarget: jobTestSetupID,
		version:     3,
		detach:      true,
		out:         &bytes.Buffer{},
		errOut:      &bytes.Buffer{},
	}
}

// writeCreatedJob answers POST /jobs with a minimal batch-shape body: id,
// name and an always-present `run` object, matching the jobs-are-jobs
// contract.
func writeCreatedJob(w http.ResponseWriter, id, name string) {
	writeData(w, map[string]any{
		"id":   id,
		"name": name,
		"run":  map[string]any{"id": "run-1", "status": "queued", "acceptedAt": "2026-09-28T00:00:00Z"},
	})
}

// The managed (non-pinned) path must never put a pinnedDeploymentId key on
// the wire at all, CreateBatchJobRequest.PinnedDeploymentID carries
// `omitempty` for exactly this, and this test asserts the wire, not just
// the parsed request struct.
func TestCreateJobOmitsPinnedDeploymentIDWhenNotPinned(t *testing.T) {
	var body []byte
	srv := jobRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "myenv")
	})
	defer srv.Close()

	opts := baseRunOpts(srv.URL)
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	if strings.Contains(string(body), "pinnedDeploymentId") {
		t.Fatalf("managed-path request body must omit pinnedDeploymentId entirely, got: %s", body)
	}
}

// The --on path must send the resolved attached deployment id verbatim, and
// only that id, never a derived or rounded value.
func TestCreateJobSendsThePinnedDeploymentIDOnTheWire(t *testing.T) {
	var body []byte
	srv := jobRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "myenv")
	})
	defer srv.Close()

	opts := baseRunOpts(srv.URL)
	opts.pinnedDeploymentID = 4242
	opts.onAlias = "lease-a"
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}

	var decoded api.CreateBatchJobRequest
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
	srv := jobRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, backendMsg)
	})
	defer srv.Close()

	opts := baseRunOpts(srv.URL)
	opts.pinnedDeploymentID = 4242
	opts.onAlias = "lease-a"
	err := runJobRun(opts)
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
	srv := jobRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusBadRequest, "name already in use")
	})
	defer srv.Close()

	opts := baseRunOpts(srv.URL)
	err := runJobRun(opts)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "could not create job") || !strings.Contains(err.Error(), "name already in use") {
		t.Fatalf("expected the managed-path wrap to survive, got: %v", err)
	}
}

// jobRun (the flag-parsing entry point) must refuse an unknown --on
// alias locally, before requireLogin or any network run, no server is
// started for this test at all.
func TestJobRunOnUnknownAliasRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--on", "ghost", "--", "true"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "aq host ls") {
		t.Fatalf("error should name `aq host ls`, got: %v", err)
	}
}

// A registered-but-never-attached alias must also be refused locally, naming
// `aq attach <alias>` as the fix.
func TestJobRunOnUnattachedAliasRefusesLocally(t *testing.T) {
	detachedSandbox(t, testHost()) // registered via `aq host add`, never attached
	err := jobRun([]string{jobTestSetupID, "3", "--on", "lease-a", "--", "true"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not attached") || !strings.Contains(err.Error(), "aq attach lease-a") {
		t.Fatalf("error should say the box is not attached and name `aq attach lease-a`, got: %v", err)
	}
}

// --max-instances and --monthly-cap-cents are DELETED, not renamed: the
// backend 400s a batch create carrying either key by name, so the CLI has no
// business accepting the flags at all any more. Passing either must fail
// loudly (Go's own "flag provided but not defined") naming the flag, never
// be silently accepted and dropped -- the same reasoning
// TestCreateJobRejectsTheDeletedSpendCapFlag already established for
// --spend-cap-cents.
func TestJobRunRejectsMaxInstances(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--max-instances", "1", "--", "true"})
	if err == nil {
		t.Fatal("expected --max-instances to be rejected, not silently accepted")
	}
	if !strings.Contains(err.Error(), "max-instances") {
		t.Fatalf("the error should name the flag that no longer exists, got: %v", err)
	}
}

func TestJobRunRejectsMonthlyCapCents(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--monthly-cap-cents", "500", "--", "true"})
	if err == nil {
		t.Fatal("expected --monthly-cap-cents to be rejected, not silently accepted")
	}
	if !strings.Contains(err.Error(), "monthly-cap-cents") {
		t.Fatalf("the error should name the flag that no longer exists, got: %v", err)
	}
}

func readAll(r *http.Request) ([]byte, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// imageRunServer answers POST /jobs (and, when a test needs it, GET
// /jobs/hardware-availability for --any-gpu) for an image-source run,
// which never resolves a setup or version and so never hits /setups.
func imageRunServer(t *testing.T, createJob http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/jobs", createJob)
	return httptest.NewServer(mux)
}

func baseImageRunOpts(serverURL string) jobRunOptions {
	return jobRunOptions{
		cred:       &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL},
		image:      "docker.io/acme/train:latest",
		gpuModels:  []string{"H100", "A100"},
		gpuCount:   1,
		diskGB:     200,
		outputPath: "/outputs",
		detach:     true,
		out:        &bytes.Buffer{},
		errOut:     &bytes.Buffer{},
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
// clause: the exact JSON an image-source `aq job run` sends must match what
// console/app/jobs/new/page.tsx's buildParams() produces for the same
// inputs, MINUS maxInstances -- the jobs-are-jobs spec deletes that key
// from a batch create's wire body entirely (the backend 400s a batch
// create that carries it), so a byte-for-byte console match on that one
// field would now assert the WRONG thing.
//
// `checkpoint` is the one field in `wantJSON` NOT read off console's
// buildParams(): console's draft carries `{paths, intervalSeconds}`, a
// shape a console-literal transcription would get wrong for this field.
// The CLI's `checkpoint{paths,exclude}` is instead the shape the
// orchestrator's own `hasCheckpointPaths`/`checkpointRequired`
// (hardware.ts:67-105) actually validates.
func TestCreateJobImageSourceMatchesConsoleBuildParams(t *testing.T) {
	var body []byte
	srv := imageRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "train")
	})
	defer srv.Close()

	opts := baseImageRunOpts(srv.URL)
	opts.registrySecret = "docker-hub"
	opts.gpuOrder = "ordered" // 2 models given, so console WOULD send this
	opts.argv = []string{"python", "train.py", "--epochs", "3"}
	opts.checkpointPaths = []string{"/workspace/checkpoints"}
	opts.checkpointExclude = []string{"/workspace/.venv"}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}

	const wantJSON = `{
		"name": "train",
		"image": {"ref": "docker.io/acme/train:latest", "registrySecret": "docker-hub"},
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
// letting the server's own checkpointRequired refusal name what is missing.
func TestCreateJobCheckpointAbsentFromWireWhenNoFlagsGiven(t *testing.T) {
	var body []byte
	srv := imageRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "train")
	})
	defer srv.Close()

	opts := baseImageRunOpts(srv.URL)
	opts.argv = []string{"python", "train.py"}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	if strings.Contains(string(body), "checkpoint") {
		t.Fatalf("checkpoint must be absent from the wire when neither flag is given, got: %s", body)
	}
}

// TestCreateJobCheckpointPresentOnWireWhenFlagsGiven asserts the wire carries
// exactly the given paths (and exclude list) once either flag is passed, on
// a VERSION-source run -- the ticket's fix applies to BOTH sources, since
// checkpointRequired runs unconditionally before any source branch.
func TestCreateJobCheckpointPresentOnWireWhenFlagsGiven(t *testing.T) {
	var body []byte
	srv := jobRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "myenv")
	})
	defer srv.Close()

	opts := baseRunOpts(srv.URL)
	opts.checkpointPaths = []string{"/workspace", "/data/out"}
	opts.checkpointExclude = []string{"/workspace/.venv", "/workspace/node_modules"}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	var decoded api.CreateBatchJobRequest
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

// TestCreateJobArgvElementWithSpacesSurvivesVerbatim: argv is taken straight
// from everything after a bare `--` and posted as a JSON array element by
// element, never shell-joined and re-split anywhere in between.
func TestCreateJobArgvElementWithSpacesSurvivesVerbatim(t *testing.T) {
	var body []byte
	srv := imageRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "train")
	})
	defer srv.Close()

	opts := baseImageRunOpts(srv.URL)
	opts.argv = []string{"python", "train.py", "--name", "my run"}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}

	var decoded api.CreateBatchJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.Entrypoint == nil || !reflect.DeepEqual(decoded.Entrypoint.Argv, opts.argv) {
		t.Fatalf("entrypoint.argv = %+v, want %v verbatim (raw body: %s)", decoded.Entrypoint, opts.argv, body)
	}
}

// TestCreateJobInstallRequirementsWrapsArgvVerbatim asserts the shared
// requirements.txt wrapper against the WIRE argv, never against
// wrapWithRequirementsInstall's own return value.
func TestCreateJobInstallRequirementsWrapsArgvVerbatim(t *testing.T) {
	var body []byte
	srv := imageRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "train")
	})
	defer srv.Close()

	opts := baseImageRunOpts(srv.URL)
	opts.argv = []string{"python", "train.py", "--epochs", "3"}
	opts.installRequirements = true
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}

	var decoded api.CreateBatchJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	wantArgv := []string{
		"bash", "-lc",
		`cp -r /inputs/. /workspace/ && pip install -q -r requirements.txt && exec "$@"`,
		"bash", "python", "train.py", "--epochs", "3",
	}
	if decoded.Entrypoint == nil || !reflect.DeepEqual(decoded.Entrypoint.Argv, wantArgv) {
		t.Fatalf("entrypoint.argv = %+v, want the literal wrapper %v (raw body: %s)", decoded.Entrypoint, wantArgv, body)
	}
}

// TestCreateJobInstallRequirementsPreservesHazardousTokensVerbatim is the
// regression the earlier `strings.Join`-based wrapper could not pass.
func TestCreateJobInstallRequirementsPreservesHazardousTokensVerbatim(t *testing.T) {
	var body []byte
	srv := imageRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "train")
	})
	defer srv.Close()

	hazardous := []string{"python", "train.py", "--run-name", "my run", "--tag", "$(whoami)"}
	opts := baseImageRunOpts(srv.URL)
	opts.argv = hazardous
	opts.installRequirements = true
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}

	var decoded api.CreateBatchJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	wantArgv := append([]string{
		"bash", "-lc",
		`cp -r /inputs/. /workspace/ && pip install -q -r requirements.txt && exec "$@"`,
		"bash",
	}, hazardous...)
	if decoded.Entrypoint == nil || !reflect.DeepEqual(decoded.Entrypoint.Argv, wantArgv) {
		t.Fatalf("entrypoint.argv = %+v, want each hazardous token intact as %v (raw body: %s)", decoded.Entrypoint, wantArgv, body)
	}
	got := decoded.Entrypoint.Argv
	if len(got) != len(wantArgv) {
		t.Fatalf("argv has %d elements, want %d (a rejoin/re-split changed the count): %v", len(got), len(wantArgv), got)
	}
	if idx := indexOf(got, "my run"); idx < 0 {
		t.Fatalf(`"my run" must survive as a single argv element, got: %v`, got)
	}
	if idx := indexOf(got, "$(whoami)"); idx < 0 {
		t.Fatalf(`"$(whoami)" must survive as a single, unexecuted argv element, got: %v`, got)
	}
}

// TestWrapWithRequirementsInstallPlacesBashAtDollarZero is the off-by-0
// regression the DELTA's revision called out by name.
func TestWrapWithRequirementsInstallPlacesBashAtDollarZero(t *testing.T) {
	argv := []string{"python", "train.py"}
	got := wrapWithRequirementsInstall(argv)

	if len(got) < 4 {
		t.Fatalf("want at least 4 elements (bash, -lc, script, $0), got %v", got)
	}
	if got[0] != "bash" || got[1] != "-lc" {
		t.Fatalf("want [bash -lc ...], got %v", got)
	}
	if got[3] != "bash" {
		t.Fatalf(`element 3 (the $0 placeholder bash -lc consumes before $1) must be the literal "bash", got %q -- omitting it eats the user's first real argument`, got[3])
	}
	if !reflect.DeepEqual(got[4:], argv) {
		t.Fatalf("argv passed to $@ = %v, want the user's own tokens %v untouched", got[4:], argv)
	}
}

// indexOf returns the index of needle in haystack, or -1.
func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

// TestJobRunInstallRequirementsWithNoCommandRefusesLocally: the flag has
// nothing to wrap without a command after `--`.
func TestJobRunInstallRequirementsWithNoCommandRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--install-requirements"})
	if err == nil {
		t.Fatal("want an error when --install-requirements is given with no command after --")
	}
	if !strings.Contains(err.Error(), "--install-requirements") {
		t.Fatalf("want the error to name the flag, got: %v", err)
	}
}

// TestCreateJobImageSourceOmitsVersionIdFromTheWire: the SOURCE XOR is
// enforced on the WIRE, not just on the parsed Go struct.
func TestCreateJobImageSourceOmitsVersionIdFromTheWire(t *testing.T) {
	var body []byte
	srv := imageRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "train")
	})
	defer srv.Close()

	opts := baseImageRunOpts(srv.URL)
	opts.argv = []string{"python", "train.py"}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	if strings.Contains(string(body), "versionId") {
		t.Fatalf("an image-source run must never put versionId on the wire, got: %s", body)
	}
}

// TestCreateJobAnyGPUFetchesHardwareAvailabilityAndSendsEveryModel:
// --any-gpu is the explicit opt-in to the console's "no card picked means
// any card" default.
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
		writeCreatedJob(w, "job-1", "train")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	opts := baseImageRunOpts(srv.URL)
	opts.gpuModels = nil
	opts.anyGPU = true
	opts.argv = []string{"python", "train.py"}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	if !availabilityHit {
		t.Fatal("--any-gpu must fetch GET /jobs/hardware-availability")
	}
	var decoded api.CreateBatchJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.Hardware == nil || !reflect.DeepEqual(decoded.Hardware.GPUModels, []string{"H100", "RTX4090"}) {
		t.Fatalf("hardware.gpuModels = %+v, want the full fetched universe [H100 RTX4090] (raw body: %s)", decoded.Hardware, body)
	}
}

// TestValidateJobGPUCount pins the closed set a Job's hardware.gpuCount
// accepts on the wire: 1, 2, 4, 8 only.
func TestValidateJobGPUCount(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8} {
		if err := validateJobGPUCount(n); err != nil {
			t.Fatalf("validateJobGPUCount(%d) = %v, want nil", n, err)
		}
	}
	for _, n := range []int{-1, 0, 3, 5, 6, 7, 9, 16} {
		err := validateJobGPUCount(n)
		if err == nil {
			t.Fatalf("validateJobGPUCount(%d) = nil, want an error", n)
		}
	}
}

// TestCreateJobImageSourceSendsRequestedGPUCount: hardware.gpuCount on the
// wire must be whatever --gpus resolved to.
func TestCreateJobImageSourceSendsRequestedGPUCount(t *testing.T) {
	var body []byte
	srv := imageRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "train")
	})
	defer srv.Close()

	opts := baseImageRunOpts(srv.URL)
	opts.gpuCount = 4
	opts.argv = []string{"torchrun", "--nproc_per_node=4", "train.py"}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	var decoded api.CreateBatchJobRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded.Hardware == nil || decoded.Hardware.GPUCount != 4 {
		t.Fatalf("hardware.gpuCount = %+v, want 4 (raw body: %s)", decoded.Hardware, body)
	}
}

// TestJobRunRejectsNonDefaultGPUsForVersionSource: a version-source run has
// no wire path to carry --gpus.
func TestJobRunRejectsNonDefaultGPUsForVersionSource(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--gpus", "2"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--gpus") || !strings.Contains(err.Error(), "--image") {
		t.Fatalf("error should name --gpus and point at --image, got: %v", err)
	}
}

// TestJobRunAllowsDefaultGPUsForVersionSource: --gpus 1 (the default) is a
// no-op on a version-source run.
func TestJobRunAllowsDefaultGPUsForVersionSource(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--gpus", "1"})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected to reach the login check with a default --gpus, got: %v", err)
	}
}

// TestJobRunRejectsGPUsWithOn: a --on pinned job rents no hardware at all.
func TestJobRunRejectsGPUsWithOn(t *testing.T) {
	detachedSandbox(t, attachedHost())
	err := jobRun([]string{jobTestSetupID, "3", "--on", "lease-a", "--gpus", "2"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--gpus") || !strings.Contains(err.Error(), "--on") {
		t.Fatalf("error should name both --gpus and --on, got: %v", err)
	}
}

// TestJobRunRejectsInvalidGPUCount: an out-of-set --gpus must be refused
// locally before any of the other source-specific checks run.
func TestJobRunRejectsInvalidGPUCount(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--gpus", "3"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--gpus must be 1, 2, 4 or 8") {
		t.Fatalf("expected the allowed-set message, got: %v", err)
	}
}

// TestCreateJobVersionSourceCommandOverridesEntrypoint: a command (argv
// after `--`) applies to a VERSION-source run too.
func TestCreateJobVersionSourceCommandOverridesEntrypoint(t *testing.T) {
	var body []byte
	srv := jobRunServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeCreatedJob(w, "job-1", "myenv")
	})
	defer srv.Close()

	opts := baseRunOpts(srv.URL)
	opts.argv = []string{"bash", "run.sh"}
	opts.outputPath = "/outputs"
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}

	got := decodeWireBody(t, body)
	entrypoint, _ := got["entrypoint"].(map[string]any)
	want := map[string]any{"kind": "command", "argv": []any{"bash", "run.sh"}, "outputPath": "/outputs"}
	if !reflect.DeepEqual(entrypoint, want) {
		t.Fatalf("entrypoint = %+v, want %+v (raw body: %s)", entrypoint, want, body)
	}
	if _, present := got["hardware"]; present {
		t.Fatalf("a version-source run must not send hardware, got: %s", body)
	}
}

// The local refusals the ticket's Tests section names, all checked with no
// server running: each must be caught before requireLogin, let alone any
// network call.

// 1. No entrypoint on an image-source run.
func TestJobRunImageWithoutEntrypointRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{"--image", "docker.io/acme/train:latest", "--gpu-model", "H100"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "must state its entrypoint") {
		t.Fatalf("error should say an entrypoint is required, got: %v", err)
	}
}

// 2. No --gpu-model (and no --any-gpu) on an image-source run.
func TestJobRunImageWithoutGPUModelRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{"--image", "docker.io/acme/train:latest", "--", "python", "train.py"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--gpu-model is required") || !strings.Contains(err.Error(), "aq gpus") {
		t.Fatalf("error should require --gpu-model and name `aq gpus`, got: %v", err)
	}
}

// 3. --registry-secret without --image.
func TestJobRunRegistrySecretWithoutImageRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--registry-secret", "docker-hub"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--registry-secret") || !strings.Contains(err.Error(), "--image") {
		t.Fatalf("error should name --registry-secret and --image, got: %v", err)
	}
}

// 4. Both sources given at once.
func TestJobRunBothSourcesRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{jobTestSetupID, "3", "--image", "docker.io/acme/train:latest"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "exactly one source") {
		t.Fatalf("error should say exactly one source is allowed, got: %v", err)
	}
}

// 5. Neither source given.
func TestJobRunNeitherSourceRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun(nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage: aq job run") {
		t.Fatalf("error should print usage, got: %v", err)
	}
}

// --gpu-model and --any-gpu together are ambiguous, refused locally.
func TestJobRunGPUModelAndAnyGPURefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{
		"--image", "docker.io/acme/train:latest", "--gpu-model", "H100", "--any-gpu",
		"--", "python", "train.py",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error should say --gpu-model and --any-gpu are mutually exclusive, got: %v", err)
	}
}

// --on pins a box that already carries a saved version, so it cannot serve
// an --image job.
func TestJobRunOnWithImageRefusesLocally(t *testing.T) {
	detachedSandbox(t, attachedHost())
	err := jobRun([]string{
		"--image", "docker.io/acme/train:latest", "--gpu-model", "H100", "--on", "lease-a",
		"--", "python", "train.py",
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "cannot serve an --image job") {
		t.Fatalf("error should say a pinned box cannot serve an --image job, got: %v", err)
	}
}

// An argv token after `--` that itself looks like a flag must reach the
// entrypoint verbatim, never be re-parsed as an aq flag.
func TestJobRunArgvLookingLikeAFlagIsNeverReparsed(t *testing.T) {
	detachedSandbox(t)
	err := jobRun([]string{
		jobTestSetupID, "3", "--", "python", "train.py", "--epochs", "3",
	})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("argv containing flag-shaped tokens should reach the login check untouched, got: %v", err)
	}
}

// jobRunFollowServer answers the calls a foreground (non --detach) run
// needs: POST /jobs to create it (with a `run` object embedded), and the log
// tail + final status GET for the created run.
func jobRunFollowServer(t *testing.T, runStatus string) (*httptest.Server, *bool) {
	t.Helper()
	logsHit := false
	mux := http.NewServeMux()
	mux.HandleFunc("/setups", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": jobTestSetupID, "name": "myenv"}})
	})
	mux.HandleFunc("/setups/versions", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": 555, "version": 3, "setup_id": jobTestSetupID}})
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeCreatedJob(w, "job-1", "myjob")
		case http.MethodGet:
			// resolveJobID (runJobLogsFollow's own lookup, sharing no client
			// instance with the create call above) needs the created job to
			// come back from a plain GET /jobs too.
			writeData(w, []map[string]any{{"id": "job-1", "name": "myjob"}})
		default:
			http.NotFound(w, r)
		}
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
	mux.HandleFunc("/jobs/job-1/runs/run-1", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"id": "run-1", "status": runStatus, "reason": ""})
	})
	return httptest.NewServer(mux), &logsHit
}

// TestRunJobRunStreamsToCompletionOnSuccess: the default (non-detach)
// behaviour creates the job, streams its log, then confirms the run's final
// status and returns nil for "succeeded".
func TestRunJobRunStreamsToCompletionOnSuccess(t *testing.T) {
	srv, logsHit := jobRunFollowServer(t, "succeeded")
	defer srv.Close()

	var out, errOut bytes.Buffer
	opts := baseRunOpts(srv.URL)
	opts.out = &out
	opts.errOut = &errOut
	opts.detach = false
	opts.sleep = func(time.Duration) {}
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	if !*logsHit {
		t.Fatal("a non-detached run must stream the log, not just create it")
	}
	if !strings.Contains(out.String(), "hello from the box") {
		t.Fatalf("want the tailed log chunk printed, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "Succeeded") {
		t.Fatalf("want the final status word announced, got: %s", out.String())
	}
}

// TestRunJobRunExitsNonZeroOnFailure: "failed"/"unservable"/"cancelled" must
// all surface as a non-nil error, which main.go's run() maps to exit 1, and
// the error text must carry the addendum's display word for the status (not
// the raw wire value) -- unservable's message must also plainly say this was
// not the owner's own code, while failed/cancelled must not carry that
// disclaimer.
func TestRunJobRunExitsNonZeroOnFailure(t *testing.T) {
	for _, status := range []string{"failed", "unservable", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			srv, _ := jobRunFollowServer(t, status)
			defer srv.Close()

			opts := baseRunOpts(srv.URL)
			opts.detach = false
			opts.sleep = func(time.Duration) {}
			err := runJobRun(opts)
			if err == nil {
				t.Fatalf("want a non-nil error for a %s run", status)
			}
			word := jobStatusWord(status)
			if !strings.Contains(err.Error(), word) {
				t.Fatalf("error should name the status word %q, got: %v", word, err)
			}
			isUnservable := status == "unservable"
			hasDisclaimer := strings.Contains(err.Error(), unservableDisclaimer)
			if isUnservable && !hasDisclaimer {
				t.Fatalf("unservable's error should carry the not-your-code disclaimer, got: %v", err)
			}
			if !isUnservable && hasDisclaimer {
				t.Fatalf("%s must not carry the unservable disclaimer, got: %v", status, err)
			}
		})
	}
}

// TestRunJobRunDetachNeverStreams: --detach must print the created job and
// return immediately, never touching the log endpoint.
func TestRunJobRunDetachNeverStreams(t *testing.T) {
	srv, logsHit := jobRunFollowServer(t, "succeeded")
	defer srv.Close()

	var out bytes.Buffer
	opts := baseRunOpts(srv.URL)
	opts.out = &out
	opts.detach = true
	if err := runJobRun(opts); err != nil {
		t.Fatalf("runJobRun: %v", err)
	}
	if *logsHit {
		t.Fatal("--detach must never read the run's log")
	}
	if !strings.Contains(out.String(), "run-1") {
		t.Fatalf("want the created run id announced, got: %s", out.String())
	}
}
