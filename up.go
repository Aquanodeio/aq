package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// Template identifiers understood by the orchestrator (ogre service templates).
const (
	templateComfyUI = "comfyui"
	templateJupyter = "torch_and_jupyter"
)

// upOptions configures runUp. login() / up() fill in the real environment;
// tests inject a base URL, fast poll interval, and a buffer writer.
type upOptions struct {
	cred         *config.Credential
	template     string
	name         string
	gpuModel     string
	gpuCount     int
	maxPrice     float64
	provider     string
	showSecrets  bool
	idlePolicy   *api.IdlePolicyUpdate // nil unless the user passed --auto-pause/--warn-after/--pause-after
	out          io.Writer
	errOut       io.Writer
	pollInterval time.Duration
	timeout      time.Duration
	now          func() time.Time
	// probe reports whether the published app URL is actually serving. Tests
	// inject a deterministic stub; runUp defaults it to httpAppReady (#234).
	probe func(string) bool
}

// up parses flags and wires the real environment into runUp.
func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	comfyui := fs.Bool("comfyui", false, "Install ComfyUI on the box (default app if you don't pick one)")
	jupyter := fs.Bool("jupyter", false, "Install Torch + Jupyter on the box instead")
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "Only rent GPUs at or below this hourly price")
	gpus := fs.Int("gpus", 0, "How many GPUs the box should have (default: 1)")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. massecompute)")
	name := fs.String("name", "", "Set the deployment's display name (default: an auto-generated name)")
	showSecrets := fs.Bool("show-secrets", false, "Echo the service password to stdout (hidden by default)")
	autoPause := fs.Bool("auto-pause", false, "Enable idle auto-pause on this deployment (off by default)")
	warnAfter := fs.String("warn-after", "", "With --auto-pause: warn after this much idle time, e.g. 30m")
	pauseAfter := fs.String("pause-after", "", "With --auto-pause: auto-pause after this much idle time, e.g. 1h")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fmt.Errorf("expected at most one target — got %s", strings.Join(positional, ", "))
	}

	if *comfyui && *jupyter {
		return errors.New("choose only one of --comfyui or --jupyter")
	}

	// `aq up host:<alias>` brings services up on a box that already exists,
	// which is a different act from the rest of this command: it rents nothing,
	// prices nothing, and never reaches the API. The only positional `aq up`
	// accepts is a detached host, because "up" against a marketplace box has
	// always meant "rent one" and must keep meaning exactly that.
	if len(positional) == 1 {
		alias, ok := parseHostTarget(positional[0])
		if !ok {
			return fmt.Errorf("`aq up` takes no deployment argument — it rents a new box. To bring services up on a box you already have, use `aq up host:<alias>`; got %q", positional[0])
		}
		var extra []string
		if *jupyter {
			extra = append(extra, "--jupyter")
		} else if *comfyui {
			extra = append(extra, "--comfyui")
		}
		return runDetached(detachedOptions{verb: "up", alias: alias, args: extra, out: os.Stdout, errOut: os.Stderr})
	}

	if err := validateGPUCount(*gpus); err != nil {
		return err
	}
	template := templateComfyUI
	if *jupyter {
		template = templateJupyter
	}

	idlePolicy, err := buildUpIdlePolicy(*autoPause, *warnAfter, *pauseAfter)
	if err != nil {
		return err
	}

	cred, err := config.Load()
	if err != nil {
		return err
	}
	if cred == nil || cred.Token == "" {
		return errors.New("not logged in — run `aq login` first")
	}

	return runUp(upOptions{
		cred:        cred,
		template:    template,
		name:        strings.TrimSpace(*name),
		gpuModel:    *gpu,
		gpuCount:    *gpus,
		maxPrice:    *maxPrice,
		provider:    *provider,
		showSecrets: *showSecrets,
		idlePolicy:  idlePolicy,
		out:         os.Stdout,
		errOut:      os.Stderr,
		now:         time.Now,
	})
}

// buildUpIdlePolicy builds the optional `idlePolicy` body for POST
// /deployments/up from `aq up`'s --auto-pause/--warn-after/--pause-after flags.
//
// It returns nil — not a struct with everything zeroed — when the user passed
// none of the three flags. A nil IdlePolicy omits the key from the request
// entirely (`json:",omitempty"` on a pointer), which the orchestrator reads as
// "no opinion, use the defaults." The console can default its idle toggle to
// checked because the user SEES the checked box and can untick it; a CLI flag
// the user never typed is invisible, so its absence must never be read as an
// explicit "off" — and it must equally never be read as an explicit "on."
func buildUpIdlePolicy(autoPause bool, warnAfterStr, pauseAfterStr string) (*api.IdlePolicyUpdate, error) {
	if !autoPause && warnAfterStr == "" && pauseAfterStr == "" {
		return nil, nil
	}

	var p api.IdlePolicyUpdate
	if autoPause {
		t := true
		p.AutoPauseEnabled = &t
	}
	if warnAfterStr != "" {
		m, err := parsePositiveMinutes("--warn-after", warnAfterStr)
		if err != nil {
			return nil, err
		}
		p.WarnAfterMinutes = &m
	}
	if pauseAfterStr != "" {
		m, err := parsePositiveMinutes("--pause-after", pauseAfterStr)
		if err != nil {
			return nil, err
		}
		p.ActAfterMinutes = &m
	}
	// Same client-side mirror of the server's warn < pause rule used by
	// `aq idle set` — fail fast rather than round-trip a doomed request.
	if p.WarnAfterMinutes != nil && p.ActAfterMinutes != nil && *p.WarnAfterMinutes >= *p.ActAfterMinutes {
		return nil, fmt.Errorf(
			"--warn-after (%s) must be less than --pause-after (%s)",
			formatMinutes(*p.WarnAfterMinutes), formatMinutes(*p.ActAfterMinutes),
		)
	}
	return &p, nil
}

// runUp drives the one-command flow: ensure an SSH key → rent the cheapest
// matching GPU + bring up the env → poll until the HTTPS URL is live.
func runUp(opts upOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = 5 * time.Second
	}
	if opts.timeout <= 0 {
		opts.timeout = 15 * time.Minute
	}
	if opts.probe == nil {
		opts.probe = httpAppReady
	}

	apiURL := resolveAPIURL(opts.cred)
	client := api.NewAuthed(apiURL, opts.cred.Token, opts.cred.TeamID)

	label := templateLabel(opts.template)

	// 1. Ensure an SSH key is registered (own-key access to the box).
	sshKeyID, err := ensureSSHKey(client, opts.out)
	if err != nil {
		return err
	}

	// 2. Rent the cheapest matching GPU + bring up the env.
	fmt.Fprintf(opts.out, "Renting the cheapest matching GPU and bringing up %s...\n", label)
	res, err := client.Up(api.UpRequest{
		Template:   opts.template,
		SSHKeyID:   sshKeyID,
		Name:       opts.name,
		GPUModel:   opts.gpuModel,
		GPUCount:   opts.gpuCount,
		MaxPrice:   opts.maxPrice,
		Provider:   opts.provider,
		IdlePolicy: opts.idlePolicy,
	})
	if err != nil {
		return fmt.Errorf("could not start deployment: %w", err)
	}
	fmt.Fprintf(opts.out, "Deployment #%d created. Provisioning (this can take a few minutes)...\n", res.DeploymentID)

	// 3. Poll until the template service URL is live.
	return waitForServiceURL(client, res.DeploymentID, label, opts.out, opts.errOut, opts.showSecrets, opts.probe, opts.pollInterval, opts.timeout, opts.now)
}

// waitForServiceURL polls a deployment until its template service URL is live,
// the deployment ends (failed/closed), or the timeout elapses. Shared by
// `aq up` and `aq deploy`.
func waitForServiceURL(
	client *api.Client,
	deploymentID int,
	label string,
	out, errOut io.Writer,
	showSecrets bool,
	probe func(string) bool,
	pollInterval, timeout time.Duration,
	now func() time.Time,
) error {
	if probe == nil {
		probe = httpAppReady
	}
	deadline := now().Add(timeout)
	first := true
	announcedURL := false
	for {
		if now().After(deadline) {
			fmt.Fprintf(out, "\nStill provisioning after %s. Check status with:\n    aq status %d\n", timeout, deploymentID)
			return errors.New("timed out waiting for the env to come up")
		}
		// Check status immediately on the first iteration; only sleep *between*
		// polls so we don't add a full interval of latency up front (#207).
		if !first {
			time.Sleep(pollInterval)
		}
		first = false

		status, err := client.DeploymentStatus(deploymentID)
		if err != nil {
			// A permanent failure (hard 4xx — auth/forbidden/not-found) will never
			// resolve, so abort fast with a diagnostic instead of spinning until the
			// timeout. Transport errors and transient 5xx hiccups keep polling (#208).
			if isPermanentStatusError(err) {
				return fmt.Errorf("could not check deployment %d status: %w", deploymentID, err)
			}
			continue
		}

		if isClosedStatus(status.Deployment.Status) {
			return fmt.Errorf("deployment %d ended with status %q before coming up", deploymentID, status.Deployment.Status)
		}

		// Once the box is up, a failed/partial server-side restore is terminal:
		// `aq deploy` would otherwise relaunch the app on missing data and the
		// template URL may never appear (the orchestrator skips the app start on a
		// failed restore). Surface it now rather than spinning out the timeout
		// (#235). A plain `aq up` never restores, so restore_status is blank and
		// this is a no-op for it.
		if isActiveStatus(status.Deployment.Status) {
			if err := restoreOutcomeError(status.Deployment); err != nil {
				return fmt.Errorf("deployment %d is up but %w", deploymentID, err)
			}
		}

		creds := status.Deployment.ServiceCredentials
		if creds != nil && creds.URL != "" {
			// ACTIVE + a published service URL does NOT mean the app is reachable:
			// the orchestrator surfaces the URL before the app binds its port, so
			// reporting "ready" here makes the user click a URL that 'connection
			// refused's. Gate the ready message on an HTTP probe that the app
			// actually answers, mirroring an app-port health check (#234).
			if !probe(creds.URL) {
				if !announcedURL {
					fmt.Fprintf(out, "Service URL published — waiting for %s to start serving...\n", label)
					announcedURL = true
				}
				continue
			}
			dep := withID(status.Deployment, deploymentID)
			// Write the alias BEFORE the ready message names it, or the first
			// thing the user copies won't resolve.
			syncManagedConfigQuiet(client, errOut, []api.Deployment{dep}, 0)
			printReady(out, errOut, label, dep, showSecrets)
			return nil
		}
	}
}

// withID backfills a deployment row's id from the id we polled with, so the
// print helpers can rely on dep.ID rather than threading it separately.
func withID(dep api.Deployment, deploymentID int) api.Deployment {
	if dep.ID == 0 {
		dep.ID = deploymentID
	}
	return dep
}

// ensureSSHKey returns the id of an SSH key the user can actually SSH in with:
// it reuses a registered key ONLY when one matches the laptop's local public
// key, otherwise it registers the local key. Reusing an arbitrary account key
// (e.g. a teammate's, on a shared account) silently breaks the own-key promise —
// the box comes up with a key the user has no private half for (#203).
func ensureSSHKey(client *api.Client, out io.Writer) (string, error) {
	key, err := resolveLocalKey()
	if err != nil {
		return "", err
	}
	path, pubKey := key.PublicPath, key.PublicKey

	keys, err := client.ListSSHKeys()
	if err != nil {
		return "", fmt.Errorf("could not list SSH keys: %w", err)
	}

	// Reuse only the registered key whose content matches the local key, so the
	// provisioned box accepts the user's private key.
	if match, ok := matchRegisteredKey(pubKey, keys); ok {
		fmt.Fprintf(out, "Using your registered SSH key %q (%s).\n", match.Name, path)
		return match.ID, nil
	}

	// No registered key matches the laptop's key — register it so the user can
	// SSH in. This covers a fresh laptop and a shared/team account whose only
	// keys belong to someone else.
	host, _ := os.Hostname()
	name := host
	if name == "" {
		name = "aq"
	}
	created, err := client.CreateSSHKey(name, pubKey)
	if err != nil {
		return "", fmt.Errorf("could not register SSH key: %w", err)
	}
	fmt.Fprintf(out, "Registered your SSH key (%s).\n", path)
	return created.ID, nil
}

// matchRegisteredKey returns the registered key whose public-key content matches
// the local key, comparing only the algorithm + base64 body so a differing
// trailing comment (e.g. user@laptop vs user@ci) doesn't defeat the match.
func matchRegisteredKey(local string, keys []api.SSHKey) (api.SSHKey, bool) {
	want := publicKeyBody(local)
	if want == "" {
		return api.SSHKey{}, false
	}
	for _, k := range keys {
		if publicKeyBody(k.PublicKey) == want {
			return k, true
		}
	}
	return api.SSHKey{}, false
}

// publicKeyBody reduces an OpenSSH public key to "<algorithm> <base64>", dropping
// the optional trailing comment, so two keys that differ only by comment compare
// equal. Returns "" if the input isn't a well-formed key line.
func publicKeyBody(key string) string {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return ""
	}
	return fields[0] + " " + fields[1]
}

func templateLabel(template string) string {
	switch template {
	case templateJupyter:
		return "Torch + Jupyter"
	default:
		return "ComfyUI"
	}
}

// isPermanentStatusError reports whether a status-poll error is a permanent
// client-side failure — a hard 4xx (401/403/404) that retrying cannot fix — as
// opposed to a transient transport error or a 5xx server hiccup during
// provisioning. Polling aborts on a permanent error instead of spinning until
// the timeout with no diagnostic; it mirrors `aq login`'s abort-on-APIError but
// keeps polling through transient 5xx (#208).
func isPermanentStatusError(err error) bool {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 400 && apiErr.Status < 500
	}
	return false
}

// isClosedStatus reports whether a deployment status is terminal (failed/closed),
// so polling can stop instead of waiting out the full timeout.
func isClosedStatus(status string) bool {
	switch status {
	case "CLOSED", "FAILED", "ERROR":
		return true
	default:
		return false
	}
}

// isActiveStatus reports whether a deployment is up and running, matching the
// ACTIVE/RUNNING check `aq deploy` uses to detect a restored box.
func isActiveStatus(status string) bool {
	switch status {
	case "ACTIVE", "RUNNING":
		return true
	default:
		return false
	}
}

// Server-side restore outcomes the orchestrator records on the deployment row
// once a snapshot restore finishes (#235).
const (
	restoreStatusSuccess = "SUCCESS"
	restoreStatusPartial = "PARTIAL"
	restoreStatusFailed  = "FAILED"
)

// restoreOutcomeError reports whether the deployment row says the server-side
// snapshot restore did NOT fully succeed, returning a user-facing error if so.
//
// `aq deploy` must report restore truth: the box reaching ACTIVE means the VM is
// up, NOT that the restore worked — a failed restore (e.g. ogre "repository does
// not exist") still leaves the box ACTIVE (#235). A blank restore_status means
// "nothing to report": a plain `aq up` never restores, and a backend that
// predates the field can't tell us — both must keep working, so only an explicit
// non-success status is treated as an error.
func restoreOutcomeError(dep api.Deployment) error {
	switch dep.RestoreStatus {
	case "", restoreStatusSuccess:
		return nil
	case restoreStatusPartial:
		msg := "the snapshot restore was incomplete — some data may be missing"
		if dep.RestoreError != "" {
			msg += " (" + dep.RestoreError + ")"
		}
		return errors.New(msg)
	default: // FAILED, or any unrecognized non-success status — fail closed
		msg := "the snapshot restore failed on the server"
		if dep.RestoreError != "" {
			msg += " (" + dep.RestoreError + ")"
		}
		return errors.New(msg)
	}
}

func printReady(out, errOut io.Writer, label string, dep api.Deployment, showSecrets bool) {
	fmt.Fprintf(out, "\n✓ %s is live:\n\n    %s\n\n", label, dep.ServiceCredentials.URL)
	printServiceCredentials(out, errOut, dep.ServiceCredentials, showSecrets, dep.ID)
	printConnection(out, dep)
	printRestoreWarnings(out, dep)
	fmt.Fprintln(out, "\nManage it in the console or run `aq whoami` to confirm your login.")
}

// printRestoreWarnings prints the server-side CUDA/vendor-skew verdict for a
// restore, when there is one. The orchestrator computes and persists
// RestoreCompatibility/RestoreWarnings on every restore, but until now no CLI
// path decoded either field — a user restoring a CUDA-12 environment onto a
// CUDA-11 box saw nothing unless the restore hard-failed. Skew stays
// non-blocking here exactly as it is server-side: the restore already reached
// ACTIVE by the time this prints, so this only warns, it never decides for the
// user. Shared by `aq up`'s printReady, `aq deploy`'s printRestored, and
// `aq import`.
func printRestoreWarnings(out io.Writer, dep api.Deployment) {
	warnings := dep.RestoreWarningMessages()
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(out, "\n⚠ Restore compatibility warnings:")
	for _, w := range warnings {
		fmt.Fprintf(out, "  - %s\n", w)
	}
}

// printConnection prints how to get a shell on the box.
//
// It names the alias rather than `ssh root@<ip>` deliberately: the alias is what
// also works with scp, rsync, and VSCode Remote-SSH, so teaching it is what
// makes those discoverable — while printing a raw IP teaches the copy-paste
// habit `aq ssh` exists to eliminate.
func printConnection(out io.Writer, dep api.Deployment) {
	host, _, ok := sshEndpointFor(dep)
	if !ok {
		return
	}
	fmt.Fprintf(out, "\n  IP:    %s\n", host)
	fmt.Fprintf(out, "  SSH:   aq ssh %s\n", sshTarget(dep))
	fmt.Fprintf(out, "  Alias: %s   (works with ssh, scp, rsync, VSCode Remote-SSH)\n", aliasFor(dep.Name, dep.ID))
}

// sshTarget is the shortest thing the user can type to reach this box.
func sshTarget(dep api.Deployment) string {
	if name := strings.TrimSpace(dep.Name); name != "" {
		return name
	}
	return strconv.Itoa(dep.ID)
}

// printServiceCredentials prints the service username to stdout and gates the
// password behind --show-secrets. By default the password is NOT echoed to
// stdout — it would otherwise land in shell scrollback, CI logs, and tee'd
// files (ticket #204). Instead a pointer to where it lives is written to errOut
// (stderr), so a piped/redirected stdout never captures the secret.
func printServiceCredentials(out, errOut io.Writer, creds *api.ServiceCredentials, showSecrets bool, deploymentID int) {
	if creds.Username != "" {
		fmt.Fprintf(out, "  Username: %s\n", creds.Username)
	}
	if creds.Password == "" {
		return
	}
	if showSecrets {
		fmt.Fprintf(out, "  Password: %s\n", creds.Password)
		return
	}
	fmt.Fprintf(errOut, "  Password: (hidden) — re-run `aq status %d --show-secrets` to print it, or view it in the console.\n", deploymentID)
}

// maxGPUCount mirrors the orchestrator's MAX_GPU_COUNT rail. Validating here
// too is not redundant: it turns a 400 from the API into a message that names
// the flag, and it costs nothing. If the server ever raises its own cap this
// becomes the binding one — which is the safe direction for a value that
// decides how expensive a box is.
const maxGPUCount = 8

// validateGPUCount rejects a --gpus value the API would refuse anyway.
//
// Zero is the unset sentinel, not a request for zero GPUs: the flag defaults to
// 0 so an unspecified request omits the field entirely and the orchestrator
// applies its own default of one.
func validateGPUCount(n int) error {
	switch {
	case n == 0:
		return nil
	case n < 0:
		return fmt.Errorf("--gpus must be a positive number, got %d", n)
	case n > maxGPUCount:
		return fmt.Errorf("--gpus is capped at %d, got %d", maxGPUCount, n)
	default:
		return nil
	}
}
