package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// deployOptions configures runDeploy. deploy() fills in the real environment;
// tests inject a base URL, fast poll interval, and a buffer writer.
type deployOptions struct {
	cred         *config.Credential
	snapshot     string
	template     string // "" → restore only, no app relaunch
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

// deploy parses flags and wires the real environment into runDeploy.
//
// `aq deploy` is the OSS→compute bridge: from a snapshot the user created with
// the standalone ogre CLI (`ogre snapshot --to aquanode`), rent the cheapest
// matching GPU on Aquanode, restore the snapshot onto it, and relaunch the app —
// one command from a free OSS snapshot to billable platform compute. See
// research/action/02-console-dx.md (CD3).
func deploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	snapshot := fs.String("snapshot", "", "Snapshot to deploy (id from `aq` / the console, e.g. ext-42)")
	comfyui := fs.Bool("comfyui", false, "Relaunch ComfyUI on the restored data (default)")
	jupyter := fs.Bool("jupyter", false, "Relaunch Torch + Jupyter on the restored data")
	noApp := fs.Bool("no-app", false, "Restore only — do not relaunch an app")
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "Only rent GPUs at or below this hourly price")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. massecompute)")
	showSecrets := fs.Bool("show-secrets", false, "Echo the service password to stdout (hidden by default)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	// Allow the snapshot as a positional arg too: `aq deploy ext-42`.
	source := *snapshot
	if source == "" && len(positional) > 0 {
		source = positional[0]
	}
	if source == "" {
		return errors.New("a snapshot is required — pass --snapshot <id> (or `aq deploy <id>`)")
	}

	if *comfyui && *jupyter {
		return errors.New("choose only one of --comfyui or --jupyter")
	}
	if *noApp && (*comfyui || *jupyter) {
		return errors.New("--no-app cannot be combined with --comfyui/--jupyter")
	}

	// Default to relaunching ComfyUI (matching `aq up`), unless the user opted
	// into Jupyter or a restore-only deploy.
	template := templateComfyUI
	switch {
	case *noApp:
		template = ""
	case *jupyter:
		template = templateJupyter
	}

	cred, err := config.Load()
	if err != nil {
		return err
	}
	if cred == nil || cred.Token == "" {
		return errors.New("not logged in — run `aq login` first")
	}

	return runDeploy(deployOptions{
		cred:        cred,
		snapshot:    source,
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

// runDeploy drives the bridge: ensure an SSH key → rent the cheapest matching
// GPU + restore the snapshot onto it → poll until the HTTPS URL is live.
func runDeploy(opts deployOptions) error {
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

	// 1. Ensure an SSH key is registered (own-key access to the restored box).
	sshKeyID, err := ensureSSHKey(client, opts.out)
	if err != nil {
		return err
	}

	// 2. Rent the cheapest matching GPU + restore the snapshot onto it.
	if opts.template != "" {
		fmt.Fprintf(opts.out, "Renting the cheapest matching GPU and restoring %s (relaunching %s)...\n", opts.snapshot, templateLabel(opts.template))
	} else {
		fmt.Fprintf(opts.out, "Renting the cheapest matching GPU and restoring %s...\n", opts.snapshot)
	}
	res, err := client.Deploy(api.DeployRequest{
		SnapshotSource: opts.snapshot,
		SSHKeyID:       sshKeyID,
		Template:       opts.template,
		GPUModel:       opts.gpuModel,
		MaxPrice:       opts.maxPrice,
		Provider:       opts.provider,
	})
	if err != nil {
		return fmt.Errorf("could not start deployment: %w", err)
	}
	fmt.Fprintf(opts.out, "Deployment #%d created. Provisioning + restoring (this can take a few minutes)...\n", res.DeploymentID)

	// 3. Poll until the service URL is live.
	//
	// A restore-only deploy (no template) never publishes a template service
	// URL, so report the box as ready as soon as it is provisioned rather than
	// waiting out the timeout.
	if opts.template == "" {
		return waitForActive(client, res.DeploymentID, opts.out, opts.pollInterval, opts.timeout, opts.now)
	}
	return waitForServiceURL(client, res.DeploymentID, templateLabel(opts.template), opts.out, opts.errOut, opts.showSecrets, opts.pollInterval, opts.timeout, opts.now)
}

// waitForActive polls a deployment until it reaches a running state (a
// restore-only deploy with no template service to expose), the deployment ends,
// or the timeout elapses.
func waitForActive(
	client *api.Client,
	deploymentID int,
	out io.Writer,
	pollInterval, timeout time.Duration,
	now func() time.Time,
) error {
	deadline := now().Add(timeout)
	first := true
	for {
		if now().After(deadline) {
			fmt.Fprintf(out, "\nStill provisioning after %s. Check status with:\n    aq status %d\n", timeout, deploymentID)
			return errors.New("timed out waiting for the box to come up")
		}
		// Check status immediately on the first iteration; only sleep *between*
		// polls so we don't add a full interval of latency up front (#207).
		if !first {
			time.Sleep(pollInterval)
		}
		first = false

		status, err := client.DeploymentStatus(deploymentID)
		if err != nil {
			// Abort fast on a permanent hard-4xx failure; keep polling through
			// transport errors and transient 5xx hiccups (#208).
			if isPermanentStatusError(err) {
				return fmt.Errorf("could not check deployment %d status: %w", deploymentID, err)
			}
			continue
		}
		if isClosedStatus(status.Deployment.Status) {
			return fmt.Errorf("deployment %d ended with status %q before coming up", deploymentID, status.Deployment.Status)
		}
		if isActiveStatus(status.Deployment.Status) {
			// The box is up — but that alone does NOT mean the restore worked. A
			// failed server-side restore (e.g. ogre "repository does not exist")
			// still leaves the deployment ACTIVE, so check the recorded restore
			// outcome before claiming success (#235).
			if err := restoreOutcomeError(status.Deployment); err != nil {
				return fmt.Errorf("deployment %d is up but %w", deploymentID, err)
			}
			printRestored(out, deploymentID, status.Deployment)
			return nil
		}
	}
}

// printRestored reports a restore-only (`aq deploy --no-app`) box as ready. When
// the deployment row carries a reachable endpoint it also prints the box IP and
// an `ssh root@…` line, so the user can connect right away instead of opening
// the console to find the address (#209).
func printRestored(out io.Writer, deploymentID int, dep api.Deployment) {
	fmt.Fprintf(out, "\n✓ Your snapshot was restored onto deployment #%d.\n", deploymentID)
	if host, port, ok := sshEndpoint(dep.AppURL); ok {
		fmt.Fprintf(out, "\n  IP:  %s\n", host)
		if port == "" || port == "22" {
			fmt.Fprintf(out, "  SSH: ssh root@%s\n", host)
		} else {
			fmt.Fprintf(out, "  SSH: ssh -p %s root@%s\n", port, host)
		}
	}
	fmt.Fprintf(out, "\nManage it in the console or run `aq whoami` to confirm your login.\n")
}

// sshEndpoint pulls the host and port from a deployment app URL like
// `http://1.2.3.4:22`. It returns ok=false for an empty or unparseable URL so
// the caller can simply omit the connection line.
func sshEndpoint(appURL string) (host, port string, ok bool) {
	if appURL == "" {
		return "", "", false
	}
	u, err := url.Parse(appURL)
	if err != nil || u.Hostname() == "" {
		return "", "", false
	}
	return u.Hostname(), u.Port(), true
}
