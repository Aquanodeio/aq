package main

import (
	"bufio"
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
	out      io.Writer
	errOut   io.Writer
	// ogrePath overrides ogre-binary discovery. Tests point it at a stub
	// script; importCmd leaves it empty so runImport resolves a real one.
	ogrePath string
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
	launch := fs.Bool("launch", false, "After import, report what launching it would rent (billable — see notes below)")
	gpu := fs.String("gpu", "", "With --launch: filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "With --launch: only rent GPUs at or below this hourly price")
	provider := fs.String("provider", "", "With --launch: restrict to a single provider (e.g. massecompute)")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runImport(importOptions{
		cred:     cred,
		dryRun:   *dryRun,
		includes: includes,
		excludes: excludes,
		name:     strings.TrimSpace(*name),
		yes:      *yes,
		launch:   *launch,
		gpuModel: *gpu,
		maxPrice: *maxPrice,
		provider: *provider,
		out:      os.Stdout,
		errOut:   os.Stderr,
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
			return errors.New("refusing to capture and upload without confirmation in a non-interactive shell — re-run with --yes")
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
	repoURL := resticRepoURL(start.Credentials, start.StoragePrefix)

	captured, err := runOgreCapture(ogrePath, repoURL, obs.Capture.MountPath, opts.includes, opts.excludes, env, opts.errOut)
	if err != nil {
		return fmt.Errorf("setup %s exists but the capture failed — restic is resumable, so re-running `aq import` will pick up where it left off: %w", start.SetupID, err)
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
		return fmt.Errorf("capture succeeded but registering the setup failed: %w", err)
	}

	fmt.Fprintf(opts.out, "\n✓ Imported into setup %s (version %d). See it with `aq setups`.\n", complete.SetupID, complete.VersionID)
	printImportWarnings(opts.out, complete.Warnings)

	if opts.launch {
		printLaunchVerdict(opts.out, obs, opts.gpuModel, opts.maxPrice, opts.provider)
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

	if len(obs.Survey.Unreadable) > 0 {
		fmt.Fprintln(out, "\nUnreadable (could not even check — not the same as \"not capturing\"):")
		for _, e := range obs.Survey.Unreadable {
			fmt.Fprintf(out, "  %-40s (%s)\n", e.Path, e.Reason)
		}
	}

	if obs.Manifest.Collected {
		fmt.Fprintln(out, "\nRecorded but not restorable (package manifest — reference only, never replayed):")
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
		fmt.Fprintln(out, "\n⚠ The survey hit a walk/time budget before it finished — sizes marked \">=\" above are floors, and some paths past the budget may be missing from either block entirely.")
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

// printLaunchVerdict reports the observed hardware --launch would hand to a
// restore, and what it would rent, BEFORE any money is spent (contract D.2).
//
// It stops there rather than actually renting: aq has no verb yet to
// install/run a setup version onto a fresh box — that flow is console-only
// today (see fork.go's identical, pre-existing gap for a forked setup), and
// the frozen wire contract for this feature defines no endpoint for it
// either. Fabricating a call against an endpoint that doesn't exist would be
// worse than admitting the gap.
func printLaunchVerdict(out io.Writer, obs api.ImportObservation, gpuModel string, maxPrice float64, provider string) {
	fmt.Fprintln(out, "\n--launch: observed hardware on the source box:")
	fmt.Fprintf(out, "  GPU:  %s x%d (vendor: %s, driver CUDA: %s, skew: %s)\n", obs.GPU.Name, obs.GPU.Count, obs.GPU.Vendor, obs.GPU.DriverCUDA, obs.GPU.Skew)

	target := obs.GPU.Name
	if gpuModel != "" {
		target = gpuModel
	}
	fmt.Fprintf(out, "  Would rent: %s", target)
	if maxPrice > 0 {
		fmt.Fprintf(out, " (max $%.2f/hr)", maxPrice)
	}
	if provider != "" {
		fmt.Fprintf(out, " on %s", provider)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "\naq has no install/run-version verb yet — bring this setup online from the console for now (`aq fork` has the same gap).")
}

// resticRepoURL builds a restic S3-repository URL for the scoped storage
// destination /setups/import/start minted. Restic's S3 backend syntax is
// `s3:<endpoint>/<bucket>/<path>`.
func resticRepoURL(creds api.ImportCredentials, storagePrefix string) string {
	endpoint := strings.TrimRight(creds.Endpoint, "/")
	bucket := strings.Trim(creds.Bucket, "/")
	prefix := strings.Trim(storagePrefix, "/")
	return fmt.Sprintf("s3:%s/%s/%s", endpoint, bucket, prefix)
}

// checkObservationSchema rejects an observation from an ogre this aq build
// doesn't know how to interpret. The contract requires every consumer to
// reject an unknown schema loudly rather than guess at an unfamiliar shape.
func checkObservationSchema(obs api.ImportObservation) error {
	if obs.Schema != api.ImportObservationSchema {
		return fmt.Errorf("this ogre reports observation schema %d, but this aq build only understands schema %d — update aq", obs.Schema, api.ImportObservationSchema)
	}
	return nil
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
	cmd.Stderr = errOut
	stdout, err := cmd.Output()
	if err != nil {
		return api.ImportObservation{}, fmt.Errorf("ogre survey failed: %w", err)
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

// runOgreCapture runs the real capture-and-upload pass. Credentials arrive via
// env, never argv — argv is world-readable through /proc on any multi-user
// box, which is exactly the kind of box `aq import` runs on. ogre's human
// progress goes to stderr, streamed straight through to the user.
func runOgreCapture(ogrePath, repoURL, mountPath string, includes, excludes []string, env []string, errOut io.Writer) (ogreCaptureOutput, error) {
	args := append([]string{"capture", "--json", "--repo", repoURL}, surveyPathArgs(includes, excludes)...)
	if mountPath != "" {
		args = append(args, "--mount-path", mountPath)
	}
	cmd := exec.Command(ogrePath, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = errOut
	stdout, err := cmd.Output()
	if err != nil {
		return ogreCaptureOutput{}, fmt.Errorf("ogre capture failed: %w", err)
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

	fmt.Fprintln(out, "No `ogre` on PATH — fetching it from Aquanode...")
	meta, err := client.OgreDownloadURL()
	if err != nil {
		return "", fmt.Errorf("could not get an ogre download URL — check your network connection: %w", err)
	}

	if err := downloadAndVerify(meta.URL, meta.SHA256, dest); err != nil {
		return "", fmt.Errorf("could not fetch ogre: %w", err)
	}
	fmt.Fprintf(out, "Installed ogre %s to %s\n", meta.Version, dest)
	return dest, nil
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
		return fmt.Errorf("checksum mismatch — expected %s, got %s (refusing to run an unverified binary)", wantSHA256, got)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("install ogre to %s: %w", dest, err)
	}
	return nil
}
