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
	startCalled       bool
	completeCalled    bool
	credentialsCalled bool
	warnings          []string
	// omitBackupID drops restic_backup_id from the /start response, so tests
	// can exercise the "server didn't return it" refusal (CONTRACT.md G).
	omitBackupID bool
}

func (s *importServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/setups/import/start", func(w http.ResponseWriter, r *http.Request) {
		s.startCalled = true
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
	mux.HandleFunc("/setups/import/credentials", func(w http.ResponseWriter, r *http.Request) {
		s.credentialsCalled = true
		writeData(w, map[string]any{
			"expires_at": "2026-08-27T00:00:00Z",
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
	if server.completeCalled {
		t.Fatal("must never call complete without a real backup id")
	}
}

// TestRunImportResumeReusesStoragePrefixAndBackupID drives a failed capture
// followed by `aq import --resume <setup-id>`, and checks the resumed
// capture targets the EXACT SAME storage_prefix/backup_id the first attempt
// used (never a fresh one — that's what lets restic dedup instead of
// restarting from zero or writing a second, orphaned copy), that credentials
// are re-minted via /setups/import/credentials, and that the local resume
// record is cleared once the resume completes.
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
	if server.completeCalled {
		t.Fatal("complete must not be called when the capture failed")
	}

	captureJSON := fmt.Sprintf(`{"ogre_snapshot_id":"snap-1","restic_snapshot_id":"r1","path":"/workspace","size":84213000,"observation":%s}`, marshalObservation(t, obs))
	okOgre, argsFile, _ := writeStubOgre(t, surveyJSON, captureJSON)

	resumeOpts := testImportOptions(cred, okOgre, &out, &errOut)
	resumeOpts.resumeSetupID = "setup-1"

	if err := runImport(resumeOpts); err != nil {
		t.Fatalf("runImport --resume: %v", err)
	}
	if !server.credentialsCalled {
		t.Fatal("expected /setups/import/credentials to be called to re-mint write creds")
	}
	if !server.completeCalled {
		t.Fatal("expected complete to be called after a successful resume capture")
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

	if _, err := loadImportResumeState("setup-1"); err == nil {
		t.Error("expected the local resume state to be cleared after a successful resume")
	}
}
