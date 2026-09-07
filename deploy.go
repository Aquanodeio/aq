package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
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
	name         string
	gpuModel     string
	gpuCount     int
	maxPrice     float64
	provider     string
	showSecrets  bool
	out          io.Writer
	errOut       io.Writer
	pollInterval time.Duration
	timeout      time.Duration
	now          func() time.Time
	// probe reports whether the published app URL is actually serving. Tests
	// inject a deterministic stub; runDeploy defaults it to httpAppReady (#234).
	probe func(string) bool
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
	snapshot := fs.String("snapshot", "", "Save to deploy (id from `aq` / the console, e.g. ext-42)")
	comfyui := fs.Bool("comfyui", false, "Relaunch ComfyUI on the restored data (default app if you don't pick one)")
	jupyter := fs.Bool("jupyter", false, "Relaunch Torch + Jupyter on the restored data instead")
	noApp := fs.Bool("no-app", false, "Restore only, do not relaunch an app")
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "Only rent GPUs at or below this hourly price")
	gpus := fs.Int("gpus", 0, "How many GPUs the box should have (default: 1)")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. massecompute)")
	name := fs.String("name", "", "Set the deployment's display name (default: an auto-generated name)")
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
		return errors.New("a snapshot is required: pass --snapshot <id> (or `aq deploy <id>`)")
	}

	if err := validateGPUCount(*gpus); err != nil {
		return err
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
		return errors.New("not logged in; run `aq login` first")
	}

	return runDeploy(deployOptions{
		cred:        cred,
		snapshot:    source,
		template:    template,
		name:        strings.TrimSpace(*name),
		gpuModel:    *gpu,
		gpuCount:    *gpus,
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
	if opts.probe == nil {
		opts.probe = httpAppReady
	}

	apiURL := resolveAPIURL(opts.cred)
	client := api.NewAuthed(apiURL, opts.cred.Token, opts.cred.TeamID)

	// 1. Ensure an SSH key is registered (own-key access to the restored box).
	sshKeyID, err := ensureSSHKey(client, opts.out)
	if err != nil {
		return err
	}

	// 2. Rent a box + restore the snapshot onto it. A resume with no
	// -provider derives its placement from the source deployment server-side
	// (see `Placement` below), so this pre-call line must stop claiming
	// "cheapest" when it does not yet know that it is -- only an explicit
	// -provider tells us anything before the response comes back.
	var prefix string
	if opts.provider != "" {
		prefix = fmt.Sprintf("Renting on %s and restoring %s", opts.provider, opts.snapshot)
	} else {
		prefix = fmt.Sprintf("Restoring %s onto a fresh box", opts.snapshot)
	}
	if opts.template != "" {
		fmt.Fprintf(opts.out, "%s (relaunching %s)...\n", prefix, templateLabel(opts.template))
	} else {
		fmt.Fprintf(opts.out, "%s...\n", prefix)
	}
	res, err := client.Deploy(api.DeployRequest{
		SnapshotSource: opts.snapshot,
		SSHKeyID:       sshKeyID,
		Template:       opts.template,
		Name:           opts.name,
		GPUModel:       opts.gpuModel,
		GPUCount:       opts.gpuCount,
		MaxPrice:       opts.maxPrice,
		Provider:       opts.provider,
	})
	if err != nil {
		return fmt.Errorf("could not start deployment: %w", err)
	}
	printPlacement(opts.out, opts.errOut, opts.snapshot, res.Placement)
	fmt.Fprintf(opts.out, "Deployment #%d created. Provisioning + restoring (this can take a few minutes)...\n", res.DeploymentID)

	// 3. Poll until the service URL is live.
	//
	// A restore-only deploy (no template) never publishes a template service
	// URL, so report the box as ready as soon as it is provisioned rather than
	// waiting out the timeout.
	if opts.template == "" {
		return waitForActive(client, res.DeploymentID, opts.out, opts.errOut, opts.pollInterval, opts.timeout, opts.now, printRestored)
	}
	return waitForServiceURL(client, res.DeploymentID, templateLabel(opts.template), opts.out, opts.errOut, opts.showSecrets, opts.probe, opts.pollInterval, opts.timeout, opts.now)
}

// printPlacement reports where a resume actually landed, once the
// response is in hand. A nil Placement (an older backend, or `aq up`'s
// request, which never sets one) prints nothing extra -- never an error.
// `explicit`/`open` also print nothing: the caller pinned every placement
// filter itself, or there was nothing to derive in the first place. A resume
// that pinned only ONE of them still reports `derived`, because the other was
// filled in from the source deployment and the user never typed it.
func printPlacement(out, errOut io.Writer, snapshotSource string, p *api.Placement) {
	if p == nil || p.Source != "derived" {
		return
	}
	// MovedReason is the single trigger. Derivation is per field, so either
	// the provider or the GPU alone can be the one that gave way, and
	// branching on MovedFrom by itself would stay silent on a GPU-only move.
	if p.MovedReason != "" {
		switch {
		case p.MovedFrom != "" && p.MovedFromGpuModel != "":
			fmt.Fprintf(errOut, "! Deployment %s ran on %s (%s), but %s.\n", snapshotSource, p.MovedFrom, p.MovedFromGpuModel, p.MovedReason)
			fmt.Fprintf(errOut, "  Placing on %s (%s) instead. Pass -provider %s to insist on it.\n", p.Provider, p.GPUModel, p.MovedFrom)
		case p.MovedFrom != "":
			fmt.Fprintf(errOut, "! Deployment %s ran on %s, but %s.\n", snapshotSource, p.MovedFrom, p.MovedReason)
			fmt.Fprintf(errOut, "  Placing on %s (%s) instead. Pass -provider %s to insist on it.\n", p.Provider, p.GPUModel, p.MovedFrom)
		default:
			fmt.Fprintf(errOut, "! Deployment %s ran on a %s, but %s.\n", snapshotSource, p.MovedFromGpuModel, p.MovedReason)
			fmt.Fprintf(errOut, "  Placing on %s (%s) instead. Pass -gpu %s to insist on it.\n", p.Provider, p.GPUModel, p.MovedFromGpuModel)
		}
		return
	}
	fmt.Fprintf(out, "Placing on %s (%s), same as deployment %s.\n", p.Provider, p.GPUModel, snapshotSource)
}

// waitForActive polls a deployment until it reaches a running state (a box with
// no template service to expose), the deployment ends, or the timeout elapses.
//
// `announce` prints the success block, because the two callers reach this state
// having done different things: `aq deploy --no-app` restored a snapshot onto
// the box, `aq up` with no app flag rented an empty one. Claiming a restore on
// a box that never had one is exactly the kind of copy this ticket is fixing.
func waitForActive(
	client *api.Client,
	deploymentID int,
	out, errOut io.Writer,
	pollInterval, timeout time.Duration,
	now func() time.Time,
	announce func(io.Writer, api.Deployment),
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
			dep := withID(status.Deployment, deploymentID)
			syncManagedConfigQuiet(client, errOut, []api.Deployment{dep}, 0)
			announce(out, dep)
			return nil
		}
	}
}

// printRestored reports a restore-only (`aq deploy --no-app`) box as ready,
// with the connection details so the user can get a shell right away instead of
// opening the console to find the address (#209).
func printRestored(out io.Writer, dep api.Deployment) {
	fmt.Fprintf(out, "\n✓ Your snapshot was restored onto deployment #%d.\n", dep.ID)
	printConnection(out, dep)
	printRestoreWarnings(out, dep)
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
