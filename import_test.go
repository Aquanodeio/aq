package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// writeStubOgre installs a tiny shell script standing in for the real ogre
// binary. It dumps its own argv and environment to files so tests can assert
// on them, then answers with surveyJSON for a `--survey-only` invocation or
// captureJSON for a `--repo` (real capture) invocation — matching CONTRACT.md
// section B's two forms.
func writeStubOgre(t *testing.T, surveyJSON, captureJSON string) (ogrePath, argsFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	envFile = filepath.Join(dir, "env.txt")
	ogrePath = filepath.Join(dir, "ogre")

	script := fmt.Sprintf(`#!/bin/sh
env > "%s"
printf '%%s\n' "$@" > "%s"
case " $* " in
  *" --survey-only "*)
    cat <<'SURVEY_EOF'
%s
SURVEY_EOF
    ;;
  *)
    cat <<'CAPTURE_EOF'
%s
CAPTURE_EOF
    ;;
esac
`, envFile, argsFile, surveyJSON, captureJSON)

	if err := os.WriteFile(ogrePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub ogre: %v", err)
	}
	return ogrePath, argsFile, envFile
}

// sampleObservation is a minimal, schema-valid ImportObservation with one
// capturing entry, one truncated (floor) "not capturing" entry, and one
// unreadable entry — enough to exercise all three survey blocks.
func sampleObservation() api.ImportObservation {
	return api.ImportObservation{
		Schema: api.ImportObservationSchema,
		Host:   api.ImportHost{Hostname: "gpu-7f2a"},
		GPU:    api.ImportGPU{Vendor: "nvidia", Name: "NVIDIA H100 80GB HBM3", Count: 1, Skew: "unknown"},
		Capture: api.ImportCapture{
			MountPath: "/workspace",
			Paths:     []string{"/workspace"},
		},
		Survey: api.ImportSurvey{
			Capturing: []api.ImportCaptureEntry{
				{Path: "/workspace", Bytes: 84213000, Source: "detected"},
			},
			NotCapturing: []api.ImportSkippedEntry{
				{Path: "/mnt/data", Bytes: 442381000000, BytesTruncated: true, Reason: "outside_detected_set"},
			},
			Unreadable: []api.ImportUnreadableEntry{
				{Path: "/root", Reason: "permission_denied"},
			},
		},
	}
}

func marshalObservation(t *testing.T, obs api.ImportObservation) string {
	t.Helper()
	b, err := json.Marshal(obs)
	if err != nil {
		t.Fatalf("marshal observation: %v", err)
	}
	return string(b)
}

// TestPrintSurveyRendersAllThreeBlocks checks the survey prints "capturing",
// "not capturing" (with a floor size on a truncated entry), and "unreadable"
// as three DISTINCT blocks — an unreadable path must never be merged into
// "not capturing" (a third state, per the contract).
func TestPrintSurveyRendersAllThreeBlocks(t *testing.T) {
	var out bytes.Buffer
	printSurvey(&out, sampleObservation())
	got := out.String()

	if !strings.Contains(got, "Capturing:") || !strings.Contains(got, "/workspace") {
		t.Errorf("missing capturing block; got:\n%s", got)
	}

	notCapturingIdx := strings.Index(got, "Not capturing:")
	unreadableIdx := strings.Index(got, "Unreadable")
	if notCapturingIdx == -1 || unreadableIdx == -1 {
		t.Fatalf("missing not-capturing or unreadable block; got:\n%s", got)
	}
	if unreadableIdx < notCapturingIdx {
		t.Fatalf("unreadable block rendered before not-capturing; got:\n%s", got)
	}

	notCapturingBlock := got[notCapturingIdx:unreadableIdx]
	if !strings.Contains(notCapturingBlock, "/mnt/data") {
		t.Errorf("not-capturing block missing /mnt/data; got:\n%s", notCapturingBlock)
	}
	if !strings.Contains(notCapturingBlock, ">= ") {
		t.Errorf("truncated size not rendered as a floor (>= N); got:\n%s", notCapturingBlock)
	}
	if strings.Contains(notCapturingBlock, "/root") {
		t.Errorf("unreadable path /root leaked into the not-capturing block; got:\n%s", notCapturingBlock)
	}

	unreadableBlock := got[unreadableIdx:]
	if !strings.Contains(unreadableBlock, "/root") || !strings.Contains(unreadableBlock, "permission_denied") {
		t.Errorf("unreadable block missing /root/permission_denied; got:\n%s", unreadableBlock)
	}
}

// TestRunOgreSurveyForwardsIncludeExclude checks --include/--exclude reach
// ogre in the survey invocation, in the order given.
func TestRunOgreSurveyForwardsIncludeExclude(t *testing.T) {
	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	ogrePath, argsFile, _ := writeStubOgre(t, surveyJSON, "{}")

	_, err := runOgreSurvey(ogrePath, []string{"/mnt/data", "/srv/env"}, []string{"/opt/venv/lib"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("runOgreSurvey: %v", err)
	}

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	args := string(argsRaw)
	for _, want := range []string{"--survey-only", "--include\n/mnt/data", "--include\n/srv/env", "--exclude\n/opt/venv/lib"} {
		if !strings.Contains(args, want) {
			t.Errorf("ogre args missing %q; got: %q", want, args)
		}
	}
}

// TestCheckObservationSchemaRejectsUnknownSchema checks an observation whose
// schema this aq build doesn't recognize is rejected loudly rather than
// silently accepted or partially interpreted.
func TestCheckObservationSchemaRejectsUnknownSchema(t *testing.T) {
	obs := sampleObservation()
	obs.Schema = 2
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	ogrePath, _, _ := writeStubOgre(t, surveyJSON, "{}")

	_, err := runOgreSurvey(ogrePath, nil, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("expected a schema-rejection error, got: %v", err)
	}
}

// TestRunOgreCapturePassesCredentialsViaEnvNotArgv checks the restic/S3
// credentials reach the child process through its environment, never as
// command-line arguments — argv is world-readable via /proc on the
// multi-user boxes this command runs on.
func TestRunOgreCapturePassesCredentialsViaEnvNotArgv(t *testing.T) {
	obs := sampleObservation()
	captureJSON := fmt.Sprintf(`{"ogre_snapshot_id":"snap-1","restic_snapshot_id":"r1","path":"/workspace","size":123,"observation":%s}`, marshalObservation(t, obs))
	ogrePath, argsFile, envFile := writeStubOgre(t, "{}", captureJSON)

	const secretPassword = "super-secret-restic-password"
	const secretKey = "AKIASECRETKEYVALUE"
	env := []string{
		"RESTIC_PASSWORD=" + secretPassword,
		"AWS_ACCESS_KEY_ID=" + secretKey,
		"AWS_SECRET_ACCESS_KEY=another-secret",
	}

	res, err := runOgreCapture(ogrePath, "s3:endpoint/bucket/prefix", "/workspace", nil, nil, env, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("runOgreCapture: %v", err)
	}
	if res.OgreSnapshotID != "snap-1" {
		t.Errorf("OgreSnapshotID = %q, want snap-1", res.OgreSnapshotID)
	}

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	if strings.Contains(string(argsRaw), secretPassword) || strings.Contains(string(argsRaw), secretKey) {
		t.Fatalf("secret leaked into argv: %q", string(argsRaw))
	}

	envRaw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if !strings.Contains(string(envRaw), secretPassword) || !strings.Contains(string(envRaw), secretKey) {
		t.Fatalf("credentials did not reach the child's environment: %q", string(envRaw))
	}
}

// importServer is a minimal fake of the /setups/import/* orchestrator routes.
type importServer struct {
	startCalled    bool
	completeCalled bool
	warnings       []string
}

func (s *importServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups/import/start", func(w http.ResponseWriter, r *http.Request) {
		s.startCalled = true
		writeData(w, map[string]any{
			"setup_id":        "setup-1",
			"storage_prefix":  "team-1/ws-abc",
			"restic_password": "resticpw",
			"import_token":    "tok-1",
			"expires_at":      "2026-08-26T00:00:00Z",
			"credentials": map[string]any{
				"endpoint":          "https://s3.example.com",
				"bucket":            "aquanode-storage",
				"access_key_id":     "AKIA...",
				"secret_access_key": "shh",
				"region":            "us-east-1",
			},
		})
	})
	mux.HandleFunc("/setups/import/complete", func(w http.ResponseWriter, r *http.Request) {
		s.completeCalled = true
		writeData(w, map[string]any{
			"setup_id":   "setup-1",
			"version_id": 7,
			"recipe":     map[string]any{},
			"warnings":   s.warnings,
		})
	})
	return mux
}

func testImportOptions(cred *config.Credential, ogrePath string, out, errOut *bytes.Buffer) importOptions {
	return importOptions{
		cred:     cred,
		yes:      true,
		out:      out,
		errOut:   errOut,
		ogrePath: ogrePath,
	}
}

// TestRunImportDryRunMakesNoStartCall checks --dry-run never calls
// /setups/import/start — nothing is captured or uploaded.
func TestRunImportDryRunMakesNoStartCall(t *testing.T) {
	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	ogrePath, _, _ := writeStubOgre(t, surveyJSON, "{}")

	server := &importServer{}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, ogrePath, &out, &errOut)
	opts.dryRun = true
	opts.yes = false // dry-run must stop before confirmation is even relevant

	if err := runImport(opts); err != nil {
		t.Fatalf("runImport --dry-run: %v", err)
	}
	if server.startCalled {
		t.Fatal("--dry-run called /setups/import/start — it must capture and upload nothing")
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected a dry-run notice; got:\n%s", out.String())
	}
}

// TestRunImportNonInteractiveWithoutYesRefuses checks a non-interactive shell
// without --yes refuses rather than guessing, and never starts the import.
func TestRunImportNonInteractiveWithoutYesRefuses(t *testing.T) {
	orig := isInteractiveStdin
	isInteractiveStdin = func() bool { return false }
	t.Cleanup(func() { isInteractiveStdin = orig })

	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	ogrePath, _, _ := writeStubOgre(t, surveyJSON, "{}")

	server := &importServer{}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, ogrePath, &out, &errOut)
	opts.yes = false

	err := runImport(opts)
	if err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("expected a non-interactive refusal, got: %v", err)
	}
	if server.startCalled {
		t.Fatal("import proceeded to /setups/import/start without confirmation")
	}
}

// TestRunImportHappyPathRegistersSetupAndPrintsWarnings drives the full flow
// (survey -> confirm via --yes -> start -> capture -> complete) against a fake
// orchestrator and stub ogre, and checks the returned warnings are printed.
func TestRunImportHappyPathRegistersSetupAndPrintsWarnings(t *testing.T) {
	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	captureJSON := fmt.Sprintf(`{"ogre_snapshot_id":"snap-1","restic_snapshot_id":"r1","path":"/workspace","size":84213000,"observation":%s}`, marshalObservation(t, obs))
	ogrePath, _, _ := writeStubOgre(t, surveyJSON, captureJSON)

	server := &importServer{warnings: []string{"template is null — DetectApp found nothing, this setup restores data-only"}}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, ogrePath, &out, &errOut)

	if err := runImport(opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if !server.startCalled || !server.completeCalled {
		t.Fatalf("expected both start and complete to be called: start=%v complete=%v", server.startCalled, server.completeCalled)
	}
	if !strings.Contains(out.String(), "setup-1") || !strings.Contains(out.String(), strconv.Itoa(7)) {
		t.Errorf("expected the new setup id and version printed; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "DetectApp found nothing") {
		t.Errorf("expected the import warning printed; got:\n%s", out.String())
	}
}
