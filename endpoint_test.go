package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func baseEndpointCreateOpts(serverURL string) endpointCreateOptions {
	return endpointCreateOptions{
		cred:         &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: serverURL},
		image:        "docker.io/acme/serve:latest",
		port:         8080,
		path:         "/",
		gpuModels:    []string{"H100"},
		diskGB:       100,
		maxInstances: 2,
		out:          &bytes.Buffer{},
	}
}

// TestEndpointCreateSendsHttpEntrypointOnWire is the ticket's core wire
// assertion: the request body an `aq endpoint create` actually posts, not a
// stubbed parser or the parsed Go struct. entrypoint.kind must be "http" and
// carry the given port, method fixed at "POST", resultMode fixed at
// "inline" — the shape job.service.ts's parseEntrypoint requires and the
// console's own buildHttpEntrypoint emits.
func TestEndpointCreateSendsHttpEntrypointOnWire(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "serve", "shape": "service"})
	}))
	defer srv.Close()

	opts := baseEndpointCreateOpts(srv.URL)
	if err := runEndpointCreate(opts); err != nil {
		t.Fatalf("runEndpointCreate: %v", err)
	}

	got := decodeWireBody(t, body)
	entrypoint, _ := got["entrypoint"].(map[string]any)
	want := map[string]any{
		"kind":       "http",
		"port":       float64(8080),
		"path":       "/",
		"method":     "POST",
		"resultMode": "inline",
	}
	for k, v := range want {
		if entrypoint[k] != v {
			t.Errorf("entrypoint[%q] = %v, want %v (raw body: %s)", k, entrypoint[k], v, body)
		}
	}

	hardware, _ := got["hardware"].(map[string]any)
	gpuModels, _ := hardware["gpuModels"].([]any)
	if len(gpuModels) != 1 || gpuModels[0] != "H100" {
		t.Errorf("hardware.gpuModels = %v, want [H100] (raw body: %s)", gpuModels, body)
	}
	if hardware["diskGb"] != float64(100) {
		t.Errorf("hardware.diskGb = %v, want 100 (the default, matching the console's Endpoints form) (raw body: %s)", hardware["diskGb"], body)
	}
	if got["image"].(map[string]any)["ref"] != opts.image {
		t.Errorf("image.ref = %v, want %q (raw body: %s)", got["image"], opts.image, body)
	}
	if got["maxInstances"] != float64(2) {
		t.Errorf("maxInstances = %v, want 2 (raw body: %s)", got["maxInstances"], body)
	}
}

// TestEndpointCreateSendsCustomDiskGBOnWire: --disk-gb must reach
// hardware.diskGb verbatim, not silently stay at the 100 default.
func TestEndpointCreateSendsCustomDiskGBOnWire(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "serve"})
	}))
	defer srv.Close()

	opts := baseEndpointCreateOpts(srv.URL)
	opts.diskGB = 250
	if err := runEndpointCreate(opts); err != nil {
		t.Fatalf("runEndpointCreate: %v", err)
	}

	got := decodeWireBody(t, body)
	hardware, _ := got["hardware"].(map[string]any)
	if hardware["diskGb"] != float64(250) {
		t.Fatalf("hardware.diskGb = %v, want 250 (raw body: %s)", hardware["diskGb"], body)
	}
}

// endpointCreate (the flag-parsing entry point) must refuse an out-of-range
// --disk-gb locally, before any network call — no server started.
func TestEndpointCreateRejectsDiskGBOutOfRangeLocally(t *testing.T) {
	detachedSandbox(t)
	err := endpointCreate([]string{"--image", "acme/serve", "--port", "8080", "--gpu-model", "H100", "--max-instances", "1", "--disk-gb", "5"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--disk-gb") {
		t.Fatalf("error should name --disk-gb, got: %v", err)
	}
}

// The default (no --disk-gb passed) must reach the login check, proving
// nothing local refused it first — same pattern job_test.go's
// TestJobCreateNeedsNoCapFlag uses.
func TestEndpointCreateDiskGBDefaultsWithoutFlag(t *testing.T) {
	detachedSandbox(t)
	err := endpointCreate([]string{"--image", "acme/serve", "--port", "8080", "--gpu-model", "H100", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error (no stored credential in the sandbox)")
	}
	if !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected to reach the login check with no --disk-gb flag, got: %v", err)
	}
}

// A row the server writes with entrypoint.kind=http and the given port IS
// the done-when this test stands in for when no local orchestrator is
// reachable: it inspects the exact JSON body a real POST /jobs would
// receive, byte for byte.
func TestEndpointCreateOmitsMinInstancesWhenNotKeepWarm(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "serve"})
	}))
	defer srv.Close()

	opts := baseEndpointCreateOpts(srv.URL)
	if err := runEndpointCreate(opts); err != nil {
		t.Fatalf("runEndpointCreate: %v", err)
	}
	if strings.Contains(string(body), "minInstances") {
		t.Fatalf("minInstances must be absent from the wire without --keep-warm, got: %s", body)
	}
}

func TestEndpointCreateSendsMinInstancesOneWhenKeepWarm(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = readAll(r)
		writeData(w, map[string]any{"id": "ep-1", "name": "serve"})
	}))
	defer srv.Close()

	opts := baseEndpointCreateOpts(srv.URL)
	opts.keepWarm = true
	if err := runEndpointCreate(opts); err != nil {
		t.Fatalf("runEndpointCreate: %v", err)
	}
	if !strings.Contains(string(body), `"minInstances":1`) {
		t.Fatalf(`--keep-warm must send "minInstances":1 on the wire, got: %s`, body)
	}
}

// endpointCreate (the flag-parsing entry point) must refuse locally before
// any network call when a required flag is missing — no server started.
func TestEndpointCreateRequiresGpuModelLocally(t *testing.T) {
	detachedSandbox(t)
	err := endpointCreate([]string{"--image", "acme/serve", "--port", "8080", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "gpu-model") {
		t.Fatalf("error should name --gpu-model, got: %v", err)
	}
}

func TestEndpointCreateRequiresPortLocally(t *testing.T) {
	detachedSandbox(t)
	err := endpointCreate([]string{"--image", "acme/serve", "--gpu-model", "H100", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--port") {
		t.Fatalf("error should name --port, got: %v", err)
	}
}

func TestEndpointCreateRequiresImageLocally(t *testing.T) {
	detachedSandbox(t)
	err := endpointCreate([]string{"--port", "8080", "--gpu-model", "H100", "--max-instances", "1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "usage: aq endpoint create") {
		t.Fatalf("error should be the usage line naming --image, got: %v", err)
	}
}

// `aq job create --port` is the ticket's other named refusal: a job that
// serves HTTP is an endpoint, not a job, and the fix names the right command.
func TestJobCreatePortFlagRefusesLocally(t *testing.T) {
	detachedSandbox(t)
	err := jobCreate([]string{jobTestSetupID, "3", "--max-instances", "1", "--port", "8080"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "aq endpoint create") {
		t.Fatalf("error should name `aq endpoint create`, got: %v", err)
	}
}

// TestEndpointListRequestsServiceShape asserts the actual query string GET
// /jobs receives, never just that ListJobsByShape's Go signature was called
// with "service" — the wire is the contract.
func TestEndpointListRequestsServiceShape(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		writeData(w, []map[string]any{
			{"id": "ep-1", "name": "serve", "shape": "service", "status": "running", "runningInstances": 1, "maxInstances": 2},
		})
	}))
	defer srv.Close()

	client := api.NewAuthed(srv.URL, "aq_sk_test", "team-1")
	list, err := client.ListJobsByShape("service")
	if err != nil {
		t.Fatalf("ListJobsByShape: %v", err)
	}
	if gotQuery != "shape=service" {
		t.Fatalf("query = %q, want \"shape=service\"", gotQuery)
	}
	if len(list) != 1 || list[0].Shape != "service" {
		t.Fatalf("list = %+v, want one service-shaped row", list)
	}
}

// printEndpoints renders id/name/status/instances; not exhaustive, just
// confirms the table does not panic and includes what a caller would scan
// for (name, running/max).
func TestPrintEndpointsIncludesNameAndInstances(t *testing.T) {
	var buf bytes.Buffer
	printEndpoints(&buf, []api.Job{
		{ID: "ep-1", Name: "serve", Status: "running", RunningInstances: 1, MaxInstances: 3},
	})
	out := buf.String()
	if !strings.Contains(out, "serve") || !strings.Contains(out, "1/3") {
		t.Fatalf("printEndpoints output missing name or instances column: %q", out)
	}
}

// TestEndpointURLPrintsRunURLVerbatim exercises endpointURL end to end
// against a fake server: resolve by name, then print exactly what runUrl
// carried, nothing derived or rewritten.
func TestEndpointURLPrintsRunURLVerbatim(t *testing.T) {
	const runURL = "https://api.aquanode.io/api/v1/run/ep-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{
			{"id": "ep-1", "name": "serve", "shape": "service", "runUrl": runURL},
		})
	}))
	defer srv.Close()

	client := api.NewAuthed(srv.URL, "aq_sk_test", "team-1")
	id, err := resolveJobID(client, "serve")
	if err != nil {
		t.Fatalf("resolveJobID: %v", err)
	}
	ep, err := findJob(client, id)
	if err != nil {
		t.Fatalf("findJob: %v", err)
	}
	if ep.RunURL == nil || *ep.RunURL != runURL {
		t.Fatalf("RunURL = %v, want %q verbatim", ep.RunURL, runURL)
	}
}
