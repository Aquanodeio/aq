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

// statusOptions configures runStatus. status() fills in the real environment;
// tests inject a base URL and a buffer writer.
type statusOptions struct {
	cred        *config.Credential
	target      string // numeric deployment id or a project id (resolved by runStatus)
	showSecrets bool
	out         io.Writer
	errOut      io.Writer
}

// status parses the deployment target and wires the real environment into runStatus.
//
// `aq status <deploymentId>` re-checks a deployment started by `aq up` — useful
// when `aq up` hits its provisioning timeout and tells the user to come back.
func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	showSecrets := fs.Bool("show-secrets", false, "Echo the service password to stdout (hidden by default)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	target, err := parseDeploymentTarget(positional, "status")
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runStatus(statusOptions{
		cred:        cred,
		target:      target,
		showSecrets: *showSecrets,
		out:         os.Stdout,
		errOut:      os.Stderr,
	})
}

// runStatus fetches the deployment's status and prints it, plus the live HTTPS
// URL + service credentials once ogre has published them.
func runStatus(opts statusOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}

	client := newControlClient(opts.cred)

	deploymentID, err := resolveDeploymentID(client, opts.target, "status")
	if err != nil {
		return err
	}

	res, err := client.DeploymentStatus(deploymentID)
	if err != nil {
		return fmt.Errorf("could not fetch status for deployment #%d: %w", deploymentID, err)
	}

	state := res.Deployment.Status
	if state == "" {
		state = res.Status
	}
	if state == "" {
		state = "UNKNOWN"
	}
	fmt.Fprintf(opts.out, "Deployment #%d: %s\n", deploymentID, state)
	fmt.Fprintf(opts.out, "Last saved: %s\n", lastSavedLabel(client, deploymentID))

	dep := withID(res.Deployment, deploymentID)

	creds := dep.ServiceCredentials
	if creds != nil && creds.URL != "" {
		fmt.Fprintf(opts.out, "\n%s is live:\n\n    %s\n\n", templateLabel(creds.Template), creds.URL)
		printServiceCredentials(opts.out, opts.errOut, creds, opts.showSecrets, deploymentID)
		syncManagedConfigQuiet(client, opts.errOut, []api.Deployment{dep}, 0)
		printConnection(opts.out, dep)
		return nil
	}

	if isClosedStatus(state) {
		fmt.Fprintf(opts.out, "\nThis deployment is no longer running.\n")
		return nil
	}

	// A restore-only deploy (`aq deploy --no-app`) never publishes service
	// credentials, so an ACTIVE/RUNNING box would otherwise fall through to the
	// provisioning message forever. Report it as ready with connection info
	// pulled from the deployment app URL instead, mirroring `aq deploy --no-app`
	// (#213, #209).
	if isActiveStatus(state) {
		syncManagedConfigQuiet(client, opts.errOut, []api.Deployment{dep}, 0)
		printStatusReady(opts.out, dep)
		return nil
	}

	fmt.Fprintf(opts.out, "\nStill provisioning — re-run `aq status %d` in a minute.\n", deploymentID)
	return nil
}

// printStatusReady reports an ACTIVE/RUNNING box that has no service credentials
// (a restore-only deploy) as ready, with the connection details so the user can
// get a shell instead of waiting on a provisioning message that never clears.
func printStatusReady(out io.Writer, dep api.Deployment) {
	fmt.Fprintf(out, "\n✓ Deployment #%d is ready.\n", dep.ID)
	printConnection(out, dep)
	fmt.Fprintf(out, "\nManage it in the console or run `aq whoami` to confirm your login.\n")
}

// requireLogin loads the stored credential, erroring if the CLI is not paired.
func requireLogin() (*config.Credential, error) {
	cred, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.Token == "" {
		return nil, errors.New("not logged in — run `aq login` first")
	}
	return cred, nil
}

// lastSavedLabel renders the deployment's true last-saved age for `aq status`.
// A failed history lookup degrades to "unknown" rather than failing the whole
// status command — the deployment's own status is still worth showing even if
// snapshot history is temporarily unreachable.
func lastSavedLabel(client *api.Client, deploymentID int) string {
	items, err := client.SnapshotHistory()
	if err != nil {
		return "unknown"
	}
	return formatLastSaved(items, deploymentID, time.Now())
}

// formatLastSaved renders the true age of the most recent snapshot for a
// deployment, or "never saved". It must never imply continuous protection:
// automated snapshots are opt-in and nothing schedules them at deploy time.
//
// A history item's owning deployment lives under Backups.DeploymentID — the
// top-level BackupID is the internal backup ROW id, not a deployment id, and an
// external/CLI snapshot (Backups == nil) never matches any deployment.
func formatLastSaved(items []api.SnapshotHistoryItem, deploymentID int, now time.Time) string {
	var newest time.Time
	for _, it := range items {
		if it.Backups == nil || it.Backups.DeploymentID != deploymentID {
			continue
		}
		t, err := time.Parse(time.RFC3339, it.CreatedAt)
		if err != nil {
			continue
		}
		if t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		return "never saved"
	}
	d := now.Sub(newest).Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
