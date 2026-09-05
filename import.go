package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// importOptions configures runImport. importCmd() fills in the real
// environment; tests inject a base URL, a stub ogre binary path, and buffer
// writers. (Named importCmd/runImport rather than the usual bare verb name —
// "import" is a Go keyword.)
type importOptions struct {
	cred     *config.Credential
	dryRun   bool
	includes []string
	excludes []string
	name     string
	yes      bool
	launch   bool
	gpuModel string
	maxPrice float64
	provider string
	// resumeSetupID, when set, skips survey/confirm/start entirely and
	// resumes a previously started (but not completed) import for this
	// setup id: re-mint credentials, re-run capture into the SAME
	// storage_prefix/backup_id (restic dedups what already landed), then
	// complete with the original single-use import token.
	resumeSetupID string
	out           io.Writer
	errOut        io.Writer
	// ogrePath overrides ogre-binary discovery. Tests point it at a stub
	// script; importCmd leaves it empty so runImport resolves a real one.
	ogrePath string
	// launchPollInterval/launchTimeout configure --launch's
	// install-then-poll-then-run wait. Tests inject a fast interval; zero
	// values default to the same cadence `aq up`/`aq deploy` poll at.
	launchPollInterval time.Duration
	launchTimeout      time.Duration
	launchNow          func() time.Time
}

// importCmd parses flags and wires the real environment into runImport.
//
// `aq import`, run ON the foreign box (RunPod, Vast, a bare-metal machine —
// anywhere that isn't Aquanode), captures its environment into a new
// Aquanode Setup with a synthesized recipe, so it can be launched on any
// provider we support. It is survey-first: nothing is captured or uploaded
// until the user has seen exactly what will and won't be, and confirmed.
func importCmd(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "Survey and print the plan; capture and upload nothing")
	var includes, excludes stringList
	fs.Var(&includes, "include", "Add a path to capture (repeatable)")
	fs.Var(&excludes, "exclude", "Drop a detected path from capture (repeatable)")
	name := fs.String("name", "", "Name the resulting setup (default: derived from the box's hostname)")
	yes := fs.Bool("yes", false, "Skip the interactive confirmation")
	launch := fs.Bool("launch", false, "After import, rent a GPU and restore onto it (billable)")
	gpu := fs.String("gpu", "", "With --launch: filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "With --launch: only rent GPUs at or below this hourly price")
	provider := fs.String("provider", "", "With --launch: restrict to a single provider (e.g. massecompute)")
	resume := fs.String("resume", "", "Resume a previously started import for this setup id (re-mints credentials; restic dedups what already landed)")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runImport(importOptions{
		cred:          cred,
		dryRun:        *dryRun,
		includes:      includes,
		excludes:      excludes,
		name:          strings.TrimSpace(*name),
		yes:           *yes,
		launch:        *launch,
		gpuModel:      *gpu,
		maxPrice:      *maxPrice,
		provider:      *provider,
		resumeSetupID: strings.TrimSpace(*resume),
		out:           os.Stdout,
		errOut:        os.Stderr,
	})
}

// runImport drives the whole flow: survey → confirm → create the setup →
// capture + upload → register the version.
func runImport(opts importOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}

	client := newControlClient(opts.cred)

	if opts.launchPollInterval <= 0 {
		opts.launchPollInterval = 5 * time.Second
	}
	if opts.launchTimeout <= 0 {
		opts.launchTimeout = 15 * time.Minute
	}
	if opts.launchNow == nil {
		opts.launchNow = time.Now
	}

	if opts.resumeSetupID != "" {
		return runImportResume(client, opts)
	}

	ogrePath := opts.ogrePath
	if ogrePath == "" {
		resolved, err := ensureOgreBinary(client, opts.out)
		if err != nil {
			return err
		}
		ogrePath = resolved
	}

	// 1. Survey — no network, no write. This is what makes the rest of the
	// flow honest: the user sees exactly what will and won't be captured
	// before anything leaves the box.
	obs, err := runOgreSurvey(ogrePath, opts.includes, opts.excludes, opts.errOut)
	if err != nil {
		return err
	}

	printSurvey(opts.out, obs)
	printHeldStorageCost(opts.out, obs)

	if opts.dryRun {
		fmt.Fprintln(opts.out, "\n--dry-run: nothing was captured or uploaded.")
		return nil
	}

	// 2. Confirm. A non-interactive shell without --yes refuses rather than
	// guessing — the same rule `aq save`'s first-name prompt follows.
	if !opts.yes {
		if !isInteractiveStdin() {
			return errors.New("refusing to capture and upload without confirmation in a non-interactive shell; re-run with --yes")
		}
		fmt.Fprint(opts.out, "\nProceed with capture and upload? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "y" && line != "yes" {
			return errors.New("import cancelled")
		}
	}

	// 3. Create the setup and mint scoped write credentials for it — a real,
	// billed, visible, deletable Setup exists from this point on, even if
	// the capture below fails or is abandoned.
	start, err := client.StartImport(api.ImportStartRequest{
		Name:      opts.name,
		MountPath: obs.Capture.MountPath,
	})
	if err != nil {
		return fmt.Errorf("could not start the import: %w", err)
	}

	fmt.Fprintf(opts.out, "\nCapturing and uploading to setup %s...\n", start.SetupID)

	// Credentials go through the child's environment, never argv — argv is
	// world-readable via /proc on a multi-user box, which is exactly the
	// kind of box `aq import` runs on.
	env := []string{
		"RESTIC_PASSWORD=" + start.ResticPassword,
		"AWS_ACCESS_KEY_ID=" + start.Credentials.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + start.Credentials.SecretAccessKey,
	}
	if start.Credentials.Region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+start.Credentials.Region)
	}

	// The server owns the restic repo layout, not aq (CONTRACT.md section G):
	// storagePrefix is what makes a setup's repo unique, and ResticBackupID
	// is the fixed trailing path segment (currently the literal "repo") for
	// every portable setup. An earlier version of this command used the
	// setup's own uuid here instead — that silently wrote to a path nothing
	// ever reads, orphaning the uploaded bytes. So this never guesses or
	// defaults it: a start response missing it is a hard, loud failure
	// before a single byte moves.
	if start.ResticBackupID == "" {
		return fmt.Errorf("setup %s was created but the orchestrator did not return a restic_backup_id, refusing to guess where in storage to write the capture (see CONTRACT.md section G); update aq or the orchestrator, then resume with `aq import --resume %s`", start.SetupID, start.SetupID)
	}

	// aq keeps NO local copy of storage_prefix/restic_backup_id/restic_password/
	// import_token: this box is rented from another vendor, we don't control
	// its disk, and it gets recycled. A restic password decrypting the whole
	// setup — left behind by an abandoned import — is a bad thing to leave on
	// someone else's hardware. If the capture below fails or is interrupted,
	// `aq import --resume <setup-id>` re-derives everything it needs from
	// POST /setups/import/credentials instead (setupImportResume below).
	captured, err := runOgreCapture(ogrePath, start.Credentials.Endpoint, start.Credentials.Bucket, start.Credentials.Region, start.StoragePrefix, start.ResticBackupID, obs.Capture.MountPath, opts.includes, opts.excludes, env, opts.errOut)
	if err != nil {
		return fmt.Errorf("setup %s exists but the capture failed, resume it with `aq import --resume %s` once fixed (restic dedups what already landed): %w", start.SetupID, start.SetupID, err)
	}

	// 4. Register the version, synthesizing a launchable recipe from what was
	// observed. The observation is forwarded exactly as ogre emitted it.
	complete, err := client.CompleteImport(api.ImportCompleteRequest{
		SetupID:        start.SetupID,
		ImportToken:    start.ImportToken,
		OgreSnapshotID: captured.OgreSnapshotID,
		Path:           captured.Path,
		Size:           captured.Size,
		Observation:    captured.Observation,
	})
	if err != nil {
		return fmt.Errorf("capture succeeded but registering the setup failed, resume with `aq import --resume %s` to retry: %w", start.SetupID, err)
	}

	fmt.Fprintf(opts.out, "\n✓ Imported into setup %s (version %d). See it with `aq setups`.\n", complete.SetupID, complete.VersionID)
	printImportWarnings(opts.out, complete.Warnings)

	if opts.launch {
		if err := launchImportedSetup(client, opts, complete.VersionID, obs); err != nil {
			return fmt.Errorf("setup %s (version %d) was imported successfully and is intact; launching it failed, so bring it online from the console instead: %w", complete.SetupID, complete.VersionID, err)
		}
	}

	return nil
}

// printSurvey renders the pre-capture report as three blocks — what will be
// captured, what won't (the actionable one, with sizes), and what couldn't
// even be read (a THIRD state, never merged into "not capturing") — plus the
// package manifest, which is recorded for reference and never restored.
func printSurvey(out io.Writer, obs api.ImportObservation) {
	fmt.Fprintln(out, "\nCapturing:")
	if len(obs.Survey.Capturing) == 0 {
		fmt.Fprintln(out, "  (nothing detected)")
	}
	for _, e := range obs.Survey.Capturing {
		fmt.Fprintf(out, "  %-40s %s\n", e.Path, formatSurveyBytes(e.Bytes, e.BytesTruncated))
	}

	fmt.Fprintln(out, "\nNot capturing:")
	if len(obs.Survey.NotCapturing) == 0 {
		fmt.Fprintln(out, "  (nothing skipped)")
	}
	for _, e := range obs.Survey.NotCapturing {
		fmt.Fprintf(out, "  %-40s %-12s (%s)\n", e.Path, formatSurveyBytes(e.Bytes, e.BytesTruncated), e.Reason)
	}
	// State the floor (contract H2): without this line the block above reads
	// as EXHAUSTIVE, when a directory under the floor appears in neither
	// list and nothing says a floor was ever applied — the same class of
	// failure as a silent skip.
	if obs.Survey.MinReportBytes > 0 {
		fmt.Fprintf(out, "  (directories under %s are not listed)\n", formatBytes(obs.Survey.MinReportBytes))
	}

	if len(obs.Survey.Unreadable) > 0 {
		fmt.Fprintln(out, "\nUnreadable (could not even check, not the same as \"not capturing\"):")
		for _, e := range obs.Survey.Unreadable {
			fmt.Fprintf(out, "  %-40s (%s)\n", e.Path, e.Reason)
		}
		// The remedy is the point (contract H1): these are not captured, and
		// the user can fix it.
		fmt.Fprintln(out, "  not captured; re-run under sudo to include them")
	}

	if obs.Manifest.Collected {
		fmt.Fprintln(out, "\nRecorded but not restorable (package manifest, reference only, never replayed):")
		for _, env := range obs.Manifest.PythonEnvs {
			suffix := ""
			if env.Truncated {
				suffix = " (truncated)"
			}
			fmt.Fprintf(out, "  %s (%s): %d packages%s\n", env.Env, env.Kind, len(env.Packages), suffix)
		}
		if sys := obs.Manifest.SystemPackages; sys.Manager != "" && sys.Manager != "none" {
			suffix := ""
			if sys.Truncated {
				suffix = " (truncated)"
			}
			fmt.Fprintf(out, "  %s packages: %d%s\n", sys.Manager, len(sys.Packages), suffix)
		}
	}

	if obs.Survey.Incomplete() {
		fmt.Fprintln(out, "\n⚠ The survey hit a walk/time budget before it finished. Sizes marked \">=\" above are floors, and some paths past the budget may be missing from either block entirely.")
	}
}

// formatSurveyBytes renders a survey entry's size, marking it as a floor
// (">= N") when the walk hit a budget partway through that path rather than
// measuring it in full.
func formatSurveyBytes(n int64, truncated bool) string {
	s := formatBytes(n)
	if truncated {
		return ">= " + s
	}
	return s
}

// formatBytes renders n in binary (1024-based) units, matching the GiB unit
// heldStorageRateLabel prices in.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printHeldStorageCost prints the held-storage rate BEFORE the mutating
// StartImport call, mirroring autosave.go's cost-before-mutation idiom — the
// user sees what turning this box into a durable Setup will cost before a
// single byte is captured or a Setup row created.
func printHeldStorageCost(out io.Writer, obs api.ImportObservation) {
	var total int64
	for _, e := range obs.Survey.Capturing {
		total += e.Bytes
	}
	floor := ""
	for _, e := range obs.Survey.Capturing {
		if e.BytesTruncated {
			floor = "at least "
			break
		}
	}
	fmt.Fprintf(out, "\nOnce imported, this setup's held storage (%s%s) is billed at %s.\n", floor, formatBytes(total), heldStorageRateLabel)
}

// printImportWarnings prints /setups/import/complete's non-blocking warnings,
// if any — e.g. a null template because DetectApp found nothing.
func printImportWarnings(out io.Writer, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(out, "\n⚠ Import warnings:")
	for _, w := range warnings {
		fmt.Fprintf(out, "  - %s\n", w)
	}
}

// runImportResume resumes a setup whose import didn't finish. aq keeps NO
// local state for this — the setup id comes from argv (the user has it: it
// was printed at /start), and POST /setups/import/credentials returns
// EVERYTHING else needed: storage_prefix, restic_backup_id, restic_password,
// scoped write credentials, and a freshly-minted (any prior one now dead)
// import_token. This is deliberate: `aq import` runs on a box rented from
// another vendor, whose disk aq does not control and which gets recycled — a
// restic password decrypting the whole setup, left behind by an abandoned
// import, would be a bad thing to leave on someone else's hardware. Resuming
// into the SAME storage_prefix/backup_id lets restic dedup what already
// landed instead of restarting from zero, and the fresh token means
// completion can never race a lingering old one into minting a second,
// parallel-billing setup.
func runImportResume(client *api.Client, opts importOptions) error {
	out := opts.out

	fmt.Fprintf(out, "Resuming import for setup %s...\n", opts.resumeSetupID)

	refreshed, err := client.RefreshImportCredentials(opts.resumeSetupID)
	if err != nil {
		return fmt.Errorf("could not resume import for setup %s: %w", opts.resumeSetupID, err)
	}
	if refreshed.ResticBackupID == "" {
		return fmt.Errorf("setup %s: the orchestrator did not return a restic_backup_id, refusing to guess where in storage to write the capture (see CONTRACT.md section G)", opts.resumeSetupID)
	}

	ogrePath := opts.ogrePath
	if ogrePath == "" {
		resolved, err := ensureOgreBinary(client, out)
		if err != nil {
			return err
		}
		ogrePath = resolved
	}

	// Credentials go through the child's environment, never argv — same rule
	// as the first attempt.
	env := []string{
		"RESTIC_PASSWORD=" + refreshed.ResticPassword,
		"AWS_ACCESS_KEY_ID=" + refreshed.Credentials.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY=" + refreshed.Credentials.SecretAccessKey,
	}
	if refreshed.Credentials.Region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+refreshed.Credentials.Region)
	}

	captured, err := runOgreCapture(ogrePath, refreshed.Credentials.Endpoint, refreshed.Credentials.Bucket, refreshed.Credentials.Region, refreshed.StoragePrefix, refreshed.ResticBackupID, "", opts.includes, opts.excludes, env, opts.errOut)
	if err != nil {
		return fmt.Errorf("resume capture failed; re-run `aq import --resume %s` again once fixed: %w", opts.resumeSetupID, err)
	}

	complete, err := client.CompleteImport(api.ImportCompleteRequest{
		SetupID:        refreshed.SetupID,
		ImportToken:    refreshed.ImportToken,
		OgreSnapshotID: captured.OgreSnapshotID,
		Path:           captured.Path,
		Size:           captured.Size,
		Observation:    captured.Observation,
	})
	if err != nil {
		return fmt.Errorf("resume capture succeeded but registering the setup failed: %w", err)
	}

	fmt.Fprintf(out, "\n✓ Resumed import into setup %s (version %d). See it with `aq setups`.\n", complete.SetupID, complete.VersionID)
	printImportWarnings(out, complete.Warnings)

	if opts.launch {
		if err := launchImportedSetup(client, opts, complete.VersionID, captured.Observation); err != nil {
			return fmt.Errorf("setup %s (version %d) was imported successfully and is intact; launching it failed, so bring it online from the console instead: %w", complete.SetupID, complete.VersionID, err)
		}
	}

	return nil
}

// launchImportedSetup rents hardware for the just-imported setup version and
// restores onto it, following setups.controller.ts's documented sequence:
// install (provision a fresh deployment FROM the recipe) → poll until active
// → run (restore the bytes onto it, passing target_deployment_id). The
// install-preview verdict is fetched and printed BEFORE anything is rented —
// it is the source doc names for "everything a prospective installer needs
// to see before renting hardware," not a price aq invents.
func launchImportedSetup(client *api.Client, opts importOptions, versionID int, obs api.ImportObservation) error {
	out := opts.out

	gpuModel := opts.gpuModel
	if gpuModel == "" {
		// contract D.2: default the GPU to the one observed on the source
		// box — matching hardware is the least surprising outcome, and the
		// preview below still lets the user see (and override) it before
		// anything is rented.
		gpuModel = obs.GPU.Name
	}

	preview, err := client.GetSetupVersionInstallPreview(versionID, gpuModel, obs.GPU.Count, 0)
	if err != nil {
		return fmt.Errorf("could not get an install preview: %w", err)
	}
	printInstallPreview(out, preview, gpuModel, opts.maxPrice, opts.provider)
	if !preview.HasRecipe {
		return errors.New("this version has no recipe to install from")
	}

	fmt.Fprintln(out, "\nThis will rent billable hardware.")
	if opts.maxPrice > 0 {
		fmt.Fprintf(out, "--max-price $%.2f/hr is the guard on that spend.\n", opts.maxPrice)
	} else {
		fmt.Fprintln(out, "No --max-price was given, the cheapest matching offer will be rented at whatever it costs.")
	}

	sshKeyID, err := ensureSSHKey(client, out)
	if err != nil {
		return err
	}

	installed, err := client.InstallSetupVersion(versionID, api.InstallSetupVersionRequest{
		SSHKeyID: sshKeyID,
		GPUModel: gpuModel,
		MaxPrice: opts.maxPrice,
		Provider: opts.provider,
	})
	if err != nil {
		return fmt.Errorf("install failed: %w", err)
	}
	fmt.Fprintf(out, "\nDeployment #%d created. Provisioning (this can take a few minutes)...\n", installed.DeploymentID)

	if err := pollUntilDeploymentActive(client, installed.DeploymentID, opts.launchPollInterval, opts.launchTimeout, opts.launchNow); err != nil {
		return fmt.Errorf("deployment #%d did not come up: %w", installed.DeploymentID, err)
	}

	ran, err := client.RunSetupVersion(versionID, api.RunSetupVersionRequest{TargetDeploymentID: installed.DeploymentID})
	if err != nil {
		return fmt.Errorf("deployment #%d is up but restoring the imported setup onto it failed: %w", installed.DeploymentID, err)
	}
	fmt.Fprintf(out, "✓ %s\n", ran.Message)
	if len(ran.Compatibility.Warnings) > 0 {
		fmt.Fprintln(out, "\n⚠ Restore compatibility warnings:")
		for _, w := range ran.Compatibility.Warnings {
			fmt.Fprintf(out, "  - %s\n", w)
		}
	}
	fmt.Fprintf(out, "\nCheck its status with `aq status %d`. Connection details appear once it's ready.\n", installed.DeploymentID)
	return nil
}

// printInstallPreview renders GET .../install-preview's verdict — template,
// the author's own hardware as a SUGGESTION only, and any warnings — plus
// what --launch would rent, all before a single dollar is spent. There is no
// price field on this endpoint (it is a compatibility/shape preview, not a
// quote), so this never invents one; `aq up`/`aq deploy` follow the same
// house rule of not pre-quoting a price before renting.
func printInstallPreview(out io.Writer, p *api.InstallPreviewResult, gpuModel string, maxPrice float64, provider string) {
	fmt.Fprintln(out, "\n--launch: install preview (before renting):")
	if !p.HasRecipe {
		fmt.Fprintln(out, "  (no recipe on this version; nothing to provision from)")
		return
	}

	template := "(none, data-only restore)"
	if p.Template != nil && *p.Template != "" {
		template = *p.Template
	}
	fmt.Fprintf(out, "  Template: %s\n", template)

	if hw := p.SuggestedHardware; hw != nil && hw.GPU != nil {
		fmt.Fprintf(out, "  Author's hardware (suggestion only): %s", *hw.GPU)
		if hw.GPUCount != nil {
			fmt.Fprintf(out, " x%d", *hw.GPUCount)
		}
		fmt.Fprintln(out)
	}
	if p.PeakVRAM != nil {
		fmt.Fprintf(out, "  Peak VRAM observed: %d MB\n", p.PeakVRAM.PeakMB)
	}

	fmt.Fprintf(out, "  Would rent: %s", gpuModel)
	if maxPrice > 0 {
		fmt.Fprintf(out, " (max $%.2f/hr)", maxPrice)
	}
	if provider != "" {
		fmt.Fprintf(out, " on %s", provider)
	}
	fmt.Fprintln(out)

	for _, w := range p.Warnings {
		fmt.Fprintf(out, "  ⚠ %s\n", w)
	}
}

// pollUntilDeploymentActive waits for a freshly-installed deployment (no
// restore triggered yet) to reach ACTIVE, so POST .../run has a real box to
// target — the poll step setups.controller.ts's install doc comment
// documents between install and run.
func pollUntilDeploymentActive(client *api.Client, deploymentID int, pollInterval, timeout time.Duration, now func() time.Time) error {
	deadline := now().Add(timeout)
	first := true
	for {
		if now().After(deadline) {
			return errors.New("timed out waiting for the box to come up")
		}
		if !first {
			time.Sleep(pollInterval)
		}
		first = false

		status, err := client.DeploymentStatus(deploymentID)
		if err != nil {
			if isPermanentStatusError(err) {
				return fmt.Errorf("could not check deployment status: %w", err)
			}
			continue
		}
		if isClosedStatus(status.Deployment.Status) {
			return fmt.Errorf("deployment ended with status %q before coming up", status.Deployment.Status)
		}
		if isActiveStatus(status.Deployment.Status) {
			return nil
		}
	}
}

// checkObservationSchema rejects an observation from an ogre this aq build
// doesn't know how to interpret. The contract requires every consumer to
// reject an unknown schema loudly rather than guess at an unfamiliar shape.
func checkObservationSchema(obs api.ImportObservation) error {
	if obs.Schema != api.ImportObservationSchema {
		return fmt.Errorf("this ogre reports observation schema %d, but this aq build only understands schema %d. Update aq", obs.Schema, api.ImportObservationSchema)
	}
	return nil
}

// ogreExitError builds a clean error from a failed ogre invocation. On a
// handler error ogre prints exactly one FINAL line to stderr — "ogre <verb>:
// <msg>" (internal/cli/cli.go) — before exiting 1, after any human progress
// lines that came before it. That final line IS the useful part (e.g.
// contract H1's "cannot read it on this box — re-run with sudo, or drop the
// flag" for an unreadable explicit --include), so it — and only it — is
// surfaced, rather than wrapping the whole transcript or a vague "exit
// status 1" behind it.
func ogreExitError(prefix string, stderr []byte, err error) error {
	lines := strings.Split(strings.TrimRight(string(stderr), "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last != "" {
		return fmt.Errorf("%s: %s", prefix, last)
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// ogreSurveyOutput mirrors `ogre capture --survey-only`'s stdout shape
// (CONTRACT.md section B): {"observation": {...}}, nothing else.
type ogreSurveyOutput struct {
	Observation api.ImportObservation `json:"observation"`
}

// ogreCaptureOutput mirrors `ogre capture`'s real (uploading) success stdout.
type ogreCaptureOutput struct {
	OgreSnapshotID   string                `json:"ogre_snapshot_id"`
	ResticSnapshotID string                `json:"restic_snapshot_id"`
	Path             string                `json:"path"`
	Size             int64                 `json:"size"`
	Observation      api.ImportObservation `json:"observation"`
}

// surveyPathArgs renders --include/--exclude flags for either ogre capture
// invocation, forwarded verbatim in the order given.
func surveyPathArgs(includes, excludes []string) []string {
	var args []string
	for _, p := range includes {
		args = append(args, "--include", p)
	}
	for _, p := range excludes {
		args = append(args, "--exclude", p)
	}
	return args
}

// runOgreSurvey runs the network-free, write-free survey pass and decodes its
// observation. ogre's human progress goes to stderr per the contract, so it's
// streamed straight through rather than captured.
func runOgreSurvey(ogrePath string, includes, excludes []string, errOut io.Writer) (api.ImportObservation, error) {
	args := append([]string{"capture", "--json", "--survey-only"}, surveyPathArgs(includes, excludes)...)
	cmd := exec.Command(ogrePath, args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(errOut, &stderrBuf)
	stdout, err := cmd.Output()
	if err != nil {
		return api.ImportObservation{}, ogreExitError("ogre survey failed", stderrBuf.Bytes(), err)
	}

	var out ogreSurveyOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		return api.ImportObservation{}, fmt.Errorf("could not parse ogre's survey output: %w", err)
	}
	if err := checkObservationSchema(out.Observation); err != nil {
		return api.ImportObservation{}, err
	}
	return out.Observation, nil
}

// runOgreCapture runs the real capture-and-upload pass. ogre builds the
// restic repo URL itself from these components (its own resticRepositoryURL
// helper, internal/vm_manage/restic_helpers.go) — aq must not hand-roll a
// second URL formatter, since the two would silently drift (CONTRACT.md F1).
// Credentials arrive via env, never argv — argv is world-readable through
// /proc on any multi-user box, which is exactly the kind of box `aq import`
// runs on. ogre's human progress goes to stderr, streamed straight through.
func runOgreCapture(ogrePath, endpoint, bucket, region, prefix, backupID, mountPath string, includes, excludes []string, env []string, errOut io.Writer) (ogreCaptureOutput, error) {
	args := []string{"capture", "--json", "--endpoint", endpoint, "--bucket", bucket, "--prefix", prefix, "--backup-id", backupID}
	if region != "" {
		args = append(args, "--region", region)
	}
	args = append(args, surveyPathArgs(includes, excludes)...)
	if mountPath != "" {
		args = append(args, "--mount-path", mountPath)
	}
	cmd := exec.Command(ogrePath, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(errOut, &stderrBuf)
	stdout, err := cmd.Output()
	if err != nil {
		return ogreCaptureOutput{}, ogreExitError("ogre capture failed", stderrBuf.Bytes(), err)
	}

	var out ogreCaptureOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		return ogreCaptureOutput{}, fmt.Errorf("could not parse ogre's capture output: %w", err)
	}
	if err := checkObservationSchema(out.Observation); err != nil {
		return ogreCaptureOutput{}, err
	}
	return out, nil
}

// ogreBinDir is where aq installs a downloaded ogre binary — never a system
// path, never with sudo. Honors XDG_DATA_HOME, defaulting to
// ~/.local/share, matching ogre's own rootless-install convention.
func ogreBinDir() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "aquanode", "bin"), nil
}

// ensureOgreBinary returns a path to a working `ogre` binary: one already on
// PATH is preferred (the same exec.LookPath idiom sshkey.go uses for
// ssh-keygen), otherwise it is fetched from the orchestrator's presigned URL,
// sha256-verified, and installed locally. This is the first time aq downloads
// and executes a helper binary — everything here fails loudly and early
// (before the foreign box is touched at all) rather than partially.
func ensureOgreBinary(client *api.Client, out io.Writer) (string, error) {
	if path, err := exec.LookPath("ogre"); err == nil {
		return path, nil
	}

	dir, err := ogreBinDir()
	if err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "ogre")
	if info, err := os.Stat(dest); err == nil && info.Mode()&0o111 != 0 {
		return dest, nil
	}

	fmt.Fprintln(out, "No `ogre` on PATH, fetching it from Aquanode...")
	meta, err := client.OgreDownloadURL()
	if err != nil {
		return "", ogreDownloadURLError(err)
	}

	if err := downloadAndVerify(meta.URL, meta.SHA256, dest); err != nil {
		return "", fmt.Errorf("could not fetch ogre: %w", err)
	}
	fmt.Fprintf(out, "Installed ogre %s to %s\n", meta.Version, dest)
	return dest, nil
}

// ogreDownloadURLError wraps a failed OgreDownloadURL call with a message that
// blames the right party. A `*api.APIError` means the orchestrator answered
// — the request reached it and it refused (its own config guard, an auth
// failure, whatever) — which is a server-side problem, never the user's
// network. Only a genuine transport failure (no HTTP response received at
// all: DNS, connection refused, TLS, timeout) is phrased as a connectivity
// issue.
func ogreDownloadURLError(err error) error {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("Aquanode could not provide an ogre download: %s", apiErr.Message)
	}
	return fmt.Errorf("could not reach Aquanode to get an ogre download URL; check your network connection: %w", err)
}

// downloadAndVerify downloads url to dest, refusing to install it unless its
// sha256 matches wantSHA256 exactly — aq must never execute an unverified
// binary it just pulled off the network.
func downloadAndVerify(url, wantSHA256, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".ogre-download-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed into place

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantSHA256) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s (refusing to run an unverified binary)", wantSHA256, got)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("install ogre to %s: %w", dest, err)
	}
	return nil
}
