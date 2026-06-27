package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	gpuModel     string
	maxPrice     float64
	provider     string
	showSecrets  bool
	out          io.Writer
	errOut       io.Writer
	pollInterval time.Duration
	timeout      time.Duration
	now          func() time.Time
}

// up parses flags and wires the real environment into runUp.
func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	comfyui := fs.Bool("comfyui", false, "Bring up ComfyUI (default)")
	jupyter := fs.Bool("jupyter", false, "Bring up Torch + Jupyter")
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "Only rent GPUs at or below this hourly price")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. massecompute)")
	showSecrets := fs.Bool("show-secrets", false, "Echo the service password to stdout (hidden by default)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *comfyui && *jupyter {
		return errors.New("choose only one of --comfyui or --jupyter")
	}
	template := templateComfyUI
	if *jupyter {
		template = templateJupyter
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
		gpuModel:    *gpu,
		maxPrice:    *maxPrice,
		provider:    *provider,
		showSecrets: *showSecrets,
		out:         os.Stdout,
		errOut:      os.Stderr,
		now:         time.Now,
	})
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

	apiURL := opts.cred.APIURL
	if apiURL == "" {
		apiURL = config.APIURL()
	}
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
		Template: opts.template,
		SSHKeyID: sshKeyID,
		GPUModel: opts.gpuModel,
		MaxPrice: opts.maxPrice,
		Provider: opts.provider,
	})
	if err != nil {
		return fmt.Errorf("could not start deployment: %w", err)
	}
	fmt.Fprintf(opts.out, "Deployment #%d created. Provisioning (this can take a few minutes)...\n", res.DeploymentID)

	// 3. Poll until the template service URL is live.
	return waitForServiceURL(client, res.DeploymentID, label, opts.out, opts.errOut, opts.showSecrets, opts.pollInterval, opts.timeout, opts.now)
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
	pollInterval, timeout time.Duration,
	now func() time.Time,
) error {
	deadline := now().Add(timeout)
	first := true
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
			printReady(out, errOut, label, creds, showSecrets, deploymentID)
			return nil
		}
	}
}

// ensureSSHKey returns the id of an SSH key the user can actually SSH in with:
// it reuses a registered key ONLY when one matches the laptop's local public
// key, otherwise it registers the local key. Reusing an arbitrary account key
// (e.g. a teammate's, on a shared account) silently breaks the own-key promise —
// the box comes up with a key the user has no private half for (#203).
func ensureSSHKey(client *api.Client, out io.Writer) (string, error) {
	path, pubKey, err := readLocalPublicKey()
	if err != nil {
		return "", err
	}

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

func printReady(out, errOut io.Writer, label string, creds *api.ServiceCredentials, showSecrets bool, deploymentID int) {
	fmt.Fprintf(out, "\n✓ %s is live:\n\n    %s\n\n", label, creds.URL)
	printServiceCredentials(out, errOut, creds, showSecrets, deploymentID)
	fmt.Fprintln(out, "\nManage it in the console or run `aq whoami` to confirm your login.")
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
