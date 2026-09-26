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

	// A detached box's status comes from the box, not from us: `ogre status` on
	// the far end reads its own GPU and snapshot state over loopback. No login,
	// no API call.
	if alias, ok := parseHostTarget(target); ok {
		return runDetached(detachedOptions{verb: "status", alias: alias, out: os.Stdout, errOut: os.Stderr})
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
	dep := withID(res.Deployment, deploymentID)

	fmt.Fprintf(opts.out, "Deployment #%d: %s\n", deploymentID, state)
	fmt.Fprintf(opts.out, "Last saved: %s\n", podSaveLabel(client, dep.SetupID, time.Now()))

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

	fmt.Fprintf(opts.out, "\nStill provisioning. Re-run `aq status %d` in a minute.\n", deploymentID)
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

// isInteractiveStdin reports whether a person is actually at the other end of
// stdin. Overridable by tests. In a non-interactive context (piped/redirected
// stdin, or a CI job) a confirmation prompt must never block, so the safe
// default (refuse, or use a default value) is used silently instead.
//
// The character-device test alone is not enough, and the gap is not academic:
// /dev/null IS a character device, so `aq <cmd> </dev/null` (the shape every
// script, CI step and automated tool runs in) reported an interactive
// terminal. Anything trusting that answer to decide whether a human could
// object was asking a proxy, and getting the confident wrong answer rather
// than the safe one. os.SameFile against os.DevNull closes it: the two stats
// agree on device+inode, so the redirect is recognised for what it is.
//
// A pipe or a regular-file redirect was always reported correctly (neither is
// a character device); /dev/null was the one hole.
var isInteractiveStdin = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if devNull, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, devNull) {
		return false
	}
	return true
}

// requireLogin loads the stored credential, erroring if the CLI is not paired.
func requireLogin() (*config.Credential, error) {
	cred, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.Token == "" {
		return nil, errors.New("not logged in; run `aq login` first")
	}
	return cred, nil
}

// podSaveLabel renders the true save status of the pod behind a deployment,
// for `aq status`'s "Last saved" line. It reads the pod's Volume summary (GET
// /setups/:id) rather than the retired GET /snapshots/history: under the
// pod/environment/volume model, Stop writes a VolumePoint and moves
// Volume.headSavedAt/saveState directly, it never mints the SnapshotVersion
// row the old lookup depended on: that made this line print "never saved"
// unconditionally, for every pod, no matter how many times it actually saved
// (confirmed live against deployments 3807 and 3809, both with a real, recent
// headSavedAt).
//
// A deployment with no SetupID, or a pod lookup that fails (the pod was since
// deleted, a transient error), degrades to "unknown" rather than failing the
// whole status command: the deployment's own status is still worth showing
// even when the pod's save state can't be confirmed right now.
func podSaveLabel(client *api.Client, setupID string, now time.Time) string {
	if setupID == "" {
		return "unknown"
	}
	pod, err := client.GetSetup(setupID)
	if err != nil {
		return "unknown"
	}
	return formatPodSaveState(pod.Volume, now)
}

// formatPodSaveState renders a pod's Volume summary as the three-state save
// status the console shows (saved / failing / unknown), never a blanket
// "never saved" that erases the difference between "no volume attached",
// "attached but nothing has landed yet", and "saving is actively failing".
func formatPodSaveState(v *api.SetupVolumeSummary, now time.Time) string {
	if v == nil {
		return "no volume attached"
	}
	if v.HeadSavedAt == nil {
		// SaveState is "unknown" whenever nothing has ever synced (see
		// SetupVolumeSummary's doc comment). HeadSavedAt nil is the honest
		// signal here, not SaveState, since saveStateOf's wire encoding
		// cannot tell "never saved" apart from "agent unreachable" on its
		// own.
		return "never saved"
	}
	t, err := time.Parse(time.RFC3339, *v.HeadSavedAt)
	if err != nil {
		return "unknown"
	}
	age := formatSavedAge(t, now)
	switch v.SaveState {
	case "failing":
		reason := "unknown reason"
		if v.LastSaveError != nil && *v.LastSaveError != "" {
			reason = *v.LastSaveError
		}
		return fmt.Sprintf("failed %s: %s", age, reason)
	case "saved":
		return age
	default:
		return "unknown"
	}
}

// formatSavedAge renders how long ago t was, at the coarsest unit that keeps
// it readable.
func formatSavedAge(t, now time.Time) string {
	d := now.Sub(t).Round(time.Minute)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
