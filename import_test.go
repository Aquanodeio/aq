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
	"time"

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

// writeStubOgreCaptureFails answers a survey-only invocation successfully but
// fails every real capture invocation — for exercising the resume path,
// where the first attempt must fail before a second (successful) attempt
// resumes it.
func writeStubOgreCaptureFails(t *testing.T, surveyJSON string) string {
	t.Helper()
	dir := t.TempDir()
	ogrePath := filepath.Join(dir, "ogre")
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" --survey-only "*)
    cat <<'SURVEY_EOF'
%s
SURVEY_EOF
    ;;
  *)
    echo "simulated capture failure" >&2
    exit 1
    ;;
esac
`, surveyJSON)
	if err := os.WriteFile(ogrePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub ogre: %v", err)
	}
	return ogrePath
}

// writeStubOgreHardError stands in for ogre exiting 1 on a handler error
// (internal/cli/cli.go's "ogre <verb>: <msg>\n" then os.Exit(1)): a few
// human progress lines, then ONE final line carrying the actual error, e.g.
// contract H1's unreadable-explicit-include remedy.
func writeStubOgreHardError(t *testing.T, finalErrLine string) string {
	t.Helper()
	dir := t.TempDir()
	ogrePath := filepath.Join(dir, "ogre")
	script := fmt.Sprintf(`#!/bin/sh
echo "installing restic rootless..." >&2
echo "surveying filesystem..." >&2
cat >&2 <<'FINAL_EOF'
%s
FINAL_EOF
exit 1
`, finalErrLine)
	if err := os.WriteFile(ogrePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub ogre: %v", err)
	}
	return ogrePath
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
			MinReportBytes: 1 << 30, // 1 GiB, ogre's default (contract H2)
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

	// Contract H1: the remedy is the point — an unreadable path can be
	// fixed by re-running under sudo, and the CLI must say so, not just
	// list the path.
	if !strings.Contains(unreadableBlock, "sudo") {
		t.Errorf("unreadable block missing the sudo remedy; got:\n%s", unreadableBlock)
	}

	// Contract H2: without the floor line, the "not capturing" block reads
	// as EXHAUSTIVE — a directory just under the floor appears in neither
	// list, and nothing says a floor was ever applied.
	if !strings.Contains(notCapturingBlock, "1.0 GiB") {
		t.Errorf("not-capturing block missing the min_report_bytes floor line; got:\n%s", notCapturingBlock)
	}
}

// TestPrintSurveyOmitsSudoRemedyWhenNothingUnreadable checks the sudo line
// only appears when there is something to fix — an empty "Unreadable" block
// must not tell the user to re-run under sudo for no reason.
func TestPrintSurveyOmitsSudoRemedyWhenNothingUnreadable(t *testing.T) {
	obs := sampleObservation()
	obs.Survey.Unreadable = nil

	var out bytes.Buffer
	printSurvey(&out, obs)
	got := out.String()

	if strings.Contains(got, "sudo") {
		t.Errorf("printed a sudo remedy with nothing unreadable; got:\n%s", got)
	}
}

// TestPrintSurveyCapturingAndUnreadableAreDistinct checks that a path
// present in Unreadable is never ALSO rendered in the Capturing block —
// contract H1: an unreadable capture root is dropped from Capturing and
// reported in Unreadable only, never both (which would otherwise claim
// capture of a tree that couldn't be read).
func TestPrintSurveyCapturingAndUnreadableAreDistinct(t *testing.T) {
	obs := api.ImportObservation{
		Schema: api.ImportObservationSchema,
		Survey: api.ImportSurvey{
			Capturing: []api.ImportCaptureEntry{
				{Path: "/opt/venv", Bytes: 1000, Source: "detected"},
			},
			Unreadable: []api.ImportUnreadableEntry{
				{Path: "/root", Reason: "permission_denied"},
			},
			MinReportBytes: 1 << 30,
		},
	}

	var out bytes.Buffer
	printSurvey(&out, obs)
	got := out.String()

	capturingIdx := strings.Index(got, "Capturing:")
	notCapturingIdx := strings.Index(got, "Not capturing:")
	capturingBlock := got[capturingIdx:notCapturingIdx]
	if strings.Contains(capturingBlock, "/root") {
		t.Errorf("an unreadable path must never also appear in Capturing; got:\n%s", capturingBlock)
	}
}

// TestRunOgreSurveySurfacesOgresFinalErrorLineCleanly checks a failed ogre
// invocation's error message is ogre's own final stderr line — e.g. contract
// H1's unreadable-explicit-include remedy ("cannot read it on this box —
// re-run with sudo, or drop the flag") — not a vague "exit status 1", and
// that human progress lines still stream live to stderr as they're printed.
func TestRunOgreSurveySurfacesOgresFinalErrorLineCleanly(t *testing.T) {
	const finalLine = "ogre capture: --include /mnt/data: cannot read it on this box — re-run with sudo, or drop the flag to import without it"
	ogrePath := writeStubOgreHardError(t, finalLine)

	var errOut bytes.Buffer
	_, err := runOgreSurvey(ogrePath, []string{"/mnt/data"}, nil, &errOut)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "re-run with sudo") {
		t.Errorf("error does not surface ogre's remedy cleanly: %v", err)
	}
	if strings.Contains(err.Error(), "exit status") {
		t.Errorf("error still wraps the vague exit-status text instead of ogre's own message: %v", err)
	}
	if !strings.Contains(errOut.String(), "installing restic rootless") {
		t.Errorf("progress lines must still stream live to stderr; got: %q", errOut.String())
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

	res, err := runOgreCapture(ogrePath, "https://s3.example.com", "aquanode-storage", "us-east-1", "team-1/ws-abc", "setup-1", "/workspace", nil, nil, env, &bytes.Buffer{})
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
	args := string(argsRaw)
	if strings.Contains(args, secretPassword) || strings.Contains(args, secretKey) {
		t.Fatalf("secret leaked into argv: %q", args)
	}
	// F1: aq must pass ogre the raw components (endpoint/bucket/region/
	// prefix/backup-id), never a hand-rolled --repo URL — ogre builds that
	// itself with its own resticRepositoryURL helper (CONTRACT.md F1).
	for _, want := range []string{"--endpoint\nhttps://s3.example.com", "--bucket\naquanode-storage", "--region\nus-east-1", "--prefix\nteam-1/ws-abc", "--backup-id\nsetup-1"} {
		if !strings.Contains(args, want) {
			t.Errorf("ogre capture args missing %q; got: %q", want, args)
		}
	}
	if strings.Contains(args, "--repo") {
		t.Errorf("--repo must not be passed — ogre builds the repo URL itself; got: %q", args)
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
	startCalls       int
	completeCalls    int
	credentialsCalls int
	warnings         []string
	// omitBackupID drops restic_backup_id from the /start response, so tests
	// can exercise the "server didn't return it" refusal (CONTRACT.md G).
	omitBackupID bool
	// lastCompleteImportToken records the import_token /complete actually
	// received, so a test can assert resume used the FRESH one from
	// /credentials rather than a remembered one from /start.
	lastCompleteImportToken string
}

func (s *importServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups/import/start", func(w http.ResponseWriter, r *http.Request) {
		s.startCalls++
		body := map[string]any{
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
		}
		if !s.omitBackupID {
			body["restic_backup_id"] = "repo"
		}
		writeData(w, body)
	})
	// /setups/import/credentials returns EVERYTHING --resume needs
	// (setup-import.service.ts's ImportCredentialsResult, landed 960c487) —
	// storage_prefix/restic_backup_id/restic_password alongside a FRESH
	// import_token, so aq keeps no local copy of any of it. The token here
	// deliberately differs from /start's "tok-1" so tests can catch aq
	// sending the wrong one to /complete.
	mux.HandleFunc("/setups/import/credentials", func(w http.ResponseWriter, r *http.Request) {
		s.credentialsCalls++
		writeData(w, map[string]any{
			"setup_id":         "setup-1",
			"storage_prefix":   "team-1/ws-abc",
			"restic_backup_id": "repo",
			"restic_password":  "resticpw",
			"import_token":     "tok-fresh",
			"expires_at":       "2026-08-27T00:00:00Z",
			"credentials": map[string]any{
				"endpoint":          "https://s3.example.com",
				"bucket":            "aquanode-storage",
				"access_key_id":     "AKIA-REFRESHED",
				"secret_access_key": "shh-refreshed",
				"region":            "us-east-1",
			},
		})
	})
	mux.HandleFunc("/setups/import/complete", func(w http.ResponseWriter, r *http.Request) {
		s.completeCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if tok, ok := body["import_token"].(string); ok {
			s.lastCompleteImportToken = tok
		}
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
	if server.startCalls != 0 {
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
	if server.startCalls != 0 {
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
	ogrePath, argsFile, _ := writeStubOgre(t, surveyJSON, captureJSON)

	server := &importServer{warnings: []string{"template is null — DetectApp found nothing, this setup restores data-only"}}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, ogrePath, &out, &errOut)

	if err := runImport(opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}
	if server.startCalls == 0 || server.completeCalls == 0 {
		t.Fatalf("expected both start and complete to be called: startCalls=%d completeCalls=%d", server.startCalls, server.completeCalls)
	}
	if !strings.Contains(out.String(), "setup-1") || !strings.Contains(out.String(), strconv.Itoa(7)) {
		t.Errorf("expected the new setup id and version printed; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "DetectApp found nothing") {
		t.Errorf("expected the import warning printed; got:\n%s", out.String())
	}

	// CONTRACT.md section G: the backup_id ogre receives must be exactly
	// whatever /setups/import/start returned ("repo" here), never a value aq
	// picked itself — an earlier version used the setup's own uuid, which
	// silently orphaned the upload.
	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	if !strings.Contains(string(argsRaw), "--backup-id\nrepo") {
		t.Errorf("capture args missing the server-provided backup id; got: %q", string(argsRaw))
	}
}

// TestRunImportWithLaunchInstallsPollsAndRuns drives the F2 --launch path end
// to end against a fake orchestrator: install-preview -> install -> poll the
// deployment to active -> run. This is the real launch primitive
// (setups.controller.ts's install/run pair), not a guessed
// DeployRequest.SnapshotSource call.
func TestRunImportWithLaunchInstallsPollsAndRuns(t *testing.T) {
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@thismachine")

	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	captureJSON := fmt.Sprintf(`{"ogre_snapshot_id":"snap-1","restic_snapshot_id":"r1","path":"/workspace","size":84213000,"observation":%s}`, marshalObservation(t, obs))
	ogrePath, _, _ := writeStubOgre(t, surveyJSON, captureJSON)

	server := &importServer{}
	mux := server.handler().(*http.ServeMux)

	var installCalled, runCalled bool
	var installBody map[string]any
	var runBody map[string]any
	statusPolls := 0

	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeData(w, []map[string]any{{"id": "key-existing", "name": "laptop", "public_key": "ssh-ed25519 AAAA laptop"}})
			return
		}
		writeData(w, map[string]any{"id": "key-new"})
	})
	mux.HandleFunc("/setups/versions/7/install-preview", func(w http.ResponseWriter, r *http.Request) {
		gpu := true
		gpuName := "NVIDIA H100 80GB HBM3"
		hasRecipe := true
		writeData(w, map[string]any{
			"id": 7, "name": "imported-box", "version": 1, "provenance": "user",
			"hasRecipe": hasRecipe, "template": nil, "image": nil, "ports": []int{},
			"hasAppUrl": false, "hasSecureUrl": false,
			"startupScript":     map[string]any{"willRun": false, "source": nil},
			"suggestedHardware": map[string]any{"gpu": gpuName, "gpuCount": 1, "cpu": nil, "memory": nil, "storage": nil},
			"peakVram":          nil,
			"warnings":          []string{},
		})
		_ = gpu
	})
	mux.HandleFunc("/setups/versions/7/install", func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		_ = json.NewDecoder(r.Body).Decode(&installBody)
		writeData(w, map[string]any{"deployment_id": 555, "project_id": "proj-1"})
	})
	mux.HandleFunc("/deployments/555/status", func(w http.ResponseWriter, r *http.Request) {
		statusPolls++
		status := "PROVISIONING"
		if statusPolls >= 2 {
			status = "ACTIVE"
		}
		writeData(w, map[string]any{"deploymentId": 555, "status": status, "deployment": map[string]any{"id": 555, "status": status}})
	})
	mux.HandleFunc("/setups/versions/7/run", func(w http.ResponseWriter, r *http.Request) {
		runCalled = true
		_ = json.NewDecoder(r.Body).Decode(&runBody)
		writeData(w, map[string]any{
			"message":       "restored",
			"compatibility": map[string]any{"warnings": []string{"driver CUDA 12.4 on the box vs 12.1 the snapshot expects"}},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, ogrePath, &out, &errOut)
	opts.launch = true
	opts.launchPollInterval = time.Millisecond

	if err := runImport(opts); err != nil {
		t.Fatalf("runImport --launch: %v", err)
	}
	if !installCalled {
		t.Fatal("expected POST /setups/versions/7/install to be called")
	}
	if !runCalled {
		t.Fatal("expected POST /setups/versions/7/run to be called")
	}
	if installBody["gpu_model"] != "NVIDIA H100 80GB HBM3" {
		t.Errorf("install body gpu_model = %v, want the observed GPU defaulted in", installBody["gpu_model"])
	}
	if id, ok := runBody["target_deployment_id"].(float64); !ok || int(id) != 555 {
		t.Errorf("run body target_deployment_id = %#v, want 555", runBody["target_deployment_id"])
	}
	if statusPolls < 2 {
		t.Errorf("expected the deployment to be polled until active, got %d polls", statusPolls)
	}
	if !strings.Contains(out.String(), "install preview") {
		t.Errorf("expected the install-preview verdict printed before renting; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "driver CUDA 12.4") {
		t.Errorf("expected the run's compatibility warning printed; got:\n%s", out.String())
	}
}

// TestRunImportRefusesWhenBackupIDMissing checks a start response with no
// restic_backup_id is a hard, loud failure — CONTRACT.md section G: the
// server owns this convention, and aq must never guess or default it (an
// earlier version of this command used the setup's own uuid, which silently
// wrote to a path nothing ever reads).
func TestRunImportRefusesWhenBackupIDMissing(t *testing.T) {
	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	ogrePath, _, _ := writeStubOgre(t, surveyJSON, "{}")

	server := &importServer{omitBackupID: true}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, ogrePath, &out, &errOut)

	err := runImport(opts)
	if err == nil || !strings.Contains(err.Error(), "restic_backup_id") {
		t.Fatalf("expected a missing-backup-id refusal, got: %v", err)
	}
	if server.completeCalls != 0 {
		t.Fatal("must never call complete without a real backup id")
	}
}

// TestRunImportResumeReusesStoragePrefixAndBackupID drives a failed capture
// followed by `aq import --resume <setup-id>`, and checks the resumed
// capture targets the SAME storage_prefix/backup_id the first attempt used —
// but sourced ENTIRELY from the /setups/import/credentials response, never
// from anything aq remembered locally (960c487 made that response return
// everything --resume needs specifically so aq keeps no local secret file on
// a box it doesn't control). Also checks the FRESH import_token from that
// response is what reaches /complete, never the original /start token.
func TestRunImportResumeReusesStoragePrefixAndBackupID(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())

	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	failingOgre := writeStubOgreCaptureFails(t, surveyJSON)

	server := &importServer{}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, failingOgre, &out, &errOut)

	err := runImport(opts)
	if err == nil || !strings.Contains(err.Error(), "--resume setup-1") {
		t.Fatalf("expected the first attempt to fail and point at --resume, got: %v", err)
	}
	if server.completeCalls != 0 {
		t.Fatal("complete must not be called when the capture failed")
	}

	// The resumed run's --resume flag is the ONLY input identifying the
	// setup — no file from the failed attempt above is read.
	captureJSON := fmt.Sprintf(`{"ogre_snapshot_id":"snap-1","restic_snapshot_id":"r1","path":"/workspace","size":84213000,"observation":%s}`, marshalObservation(t, obs))
	okOgre, argsFile, _ := writeStubOgre(t, surveyJSON, captureJSON)

	resumeOpts := testImportOptions(cred, okOgre, &out, &errOut)
	resumeOpts.resumeSetupID = "setup-1"

	if err := runImport(resumeOpts); err != nil {
		t.Fatalf("runImport --resume: %v", err)
	}
	if server.credentialsCalls == 0 {
		t.Fatal("expected /setups/import/credentials to be called to re-mint everything needed")
	}
	if server.completeCalls == 0 {
		t.Fatal("expected complete to be called after a successful resume capture")
	}
	if server.startCalls != 1 {
		t.Fatalf("resume must not call /setups/import/start again; startCalls=%d", server.startCalls)
	}
	if server.lastCompleteImportToken != "tok-fresh" {
		t.Errorf("complete used import_token %q, want the fresh one from /credentials (\"tok-fresh\"), not the original /start token", server.lastCompleteImportToken)
	}

	argsRaw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	for _, want := range []string{"--prefix\nteam-1/ws-abc", "--backup-id\nrepo"} {
		if !strings.Contains(string(argsRaw), want) {
			t.Errorf("resumed capture args missing %q; got: %q", want, string(argsRaw))
		}
	}
}

// listFilesUnder returns every regular file under dir, relative to dir. Used
// to snapshot a directory tree before/after an operation.
func listFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = path
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return files
}

// TestRunImportWritesNoFileUnderConfigDir is the regression guard for the
// box-side secret file this command used to write: it walks the config dir
// before and after a full import (survey -> confirm -> start -> capture ->
// complete) and asserts the set of files is unchanged. A filename-specific
// check would stop catching this the moment the file got renamed; this
// doesn't care what it would have been called.
func TestRunImportWritesNoFileUnderConfigDir(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AQ_CONFIG_DIR", configDir)

	before := listFilesUnder(t, configDir)

	obs := sampleObservation()
	surveyJSON := fmt.Sprintf(`{"observation": %s}`, marshalObservation(t, obs))
	captureJSON := fmt.Sprintf(`{"ogre_snapshot_id":"snap-1","restic_snapshot_id":"r1","path":"/workspace","size":84213000,"observation":%s}`, marshalObservation(t, obs))
	ogrePath, _, _ := writeStubOgre(t, surveyJSON, captureJSON)

	server := &importServer{}
	srv := httptest.NewServer(server.handler())
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out, errOut bytes.Buffer
	opts := testImportOptions(cred, ogrePath, &out, &errOut)

	if err := runImport(opts); err != nil {
		t.Fatalf("runImport: %v", err)
	}

	after := listFilesUnder(t, configDir)
	if !slicesEqualUnordered(before, after) {
		t.Errorf("files under the config dir changed during import: before=%v after=%v — aq must never write restic_password/import_token to disk on a box it doesn't control", before, after)
	}
}

// slicesEqualUnordered reports whether a and b contain the same elements,
// ignoring order.
func slicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
