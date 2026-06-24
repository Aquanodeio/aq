package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
	out          io.Writer
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
		cred:     cred,
		template: template,
		gpuModel: *gpu,
		maxPrice: *maxPrice,
		provider: *provider,
		out:      os.Stdout,
		now:      time.Now,
	})
}

// runUp drives the one-command flow: ensure an SSH key → rent the cheapest
// matching GPU + bring up the env → poll until the HTTPS URL is live.
func runUp(opts upOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
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
	return waitForServiceURL(client, res.DeploymentID, label, opts.out, opts.pollInterval, opts.timeout, opts.now)
}

// waitForServiceURL polls a deployment until its template service URL is live,
// the deployment ends (failed/closed), or the timeout elapses. Shared by
// `aq up` and `aq deploy`.
func waitForServiceURL(
	client *api.Client,
	deploymentID int,
	label string,
	out io.Writer,
	pollInterval, timeout time.Duration,
	now func() time.Time,
) error {
	deadline := now().Add(timeout)
	for {
		if now().After(deadline) {
			fmt.Fprintf(out, "\nStill provisioning after %s. Check status with:\n    aq status %d\n", timeout, deploymentID)
			return errors.New("timed out waiting for the env to come up")
		}
		time.Sleep(pollInterval)

		status, err := client.DeploymentStatus(deploymentID)
		if err != nil {
			// Transient status errors shouldn't kill the wait; keep polling.
			continue
		}

		if isClosedStatus(status.Deployment.Status) {
			return fmt.Errorf("deployment %d ended with status %q before coming up", deploymentID, status.Deployment.Status)
		}

		creds := status.Deployment.ServiceCredentials
		if creds != nil && creds.URL != "" {
			printReady(out, label, creds)
			return nil
		}
	}
}

// ensureSSHKey returns the id of an SSH key to use, registering the laptop's
// local public key if the account has none yet.
func ensureSSHKey(client *api.Client, out io.Writer) (string, error) {
	keys, err := client.ListSSHKeys()
	if err != nil {
		return "", fmt.Errorf("could not list SSH keys: %w", err)
	}
	if len(keys) > 0 {
		return keys[0].ID, nil
	}

	path, pubKey, err := readLocalPublicKey()
	if err != nil {
		return "", err
	}
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

func templateLabel(template string) string {
	switch template {
	case templateJupyter:
		return "Torch + Jupyter"
	default:
		return "ComfyUI"
	}
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

func printReady(out io.Writer, label string, creds *api.ServiceCredentials) {
	fmt.Fprintf(out, "\n✓ %s is live:\n\n    %s\n\n", label, creds.URL)
	if creds.Username != "" {
		fmt.Fprintf(out, "  Username: %s\n", creds.Username)
	}
	if creds.Password != "" {
		fmt.Fprintf(out, "  Password: %s\n", creds.Password)
	}
	fmt.Fprintln(out, "\nManage it in the console or run `aq whoami` to confirm your login.")
}
