package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// parkOptions configures runPark. park() fills in the real environment;
// tests call runPark directly.
type parkOptions struct {
	cred   *config.Credential
	target string // setup id (uuid) or name
	out    io.Writer
}

// park parses `aq park <setup>` and wires the real environment into runPark.
//
// `aq park` saves the setup's current state, then releases its rented box —
// the existing project pause route already snapshots before it lands the
// deployment PAUSED, so this is a thin CLI wrapper over that guarantee, not a
// second save path. Unlike `aq down`, parking keeps the project's one
// resumable slot: a future `aq up`/`aq deploy` against the same setup picks
// it back up automatically.
//
// Named "park", not "pause": console itself uses "pause"/"resume" for a
// DIFFERENT, older feature — SnapshotterTab's usePauseSnapshot/
// useResumeSnapshot stop/start the per-path automated-snapshot cron
// (POST /snapshots/:deploymentId/stop|start), a per-path scheduling toggle
// with nothing to do with releasing a machine. Console's own newest copy for
// THIS action (save-then-release) is "Park" — see SaveBeforeStopDialog.tsx's
// "Park {name}?" title and "Save and park" button. Reusing "pause" here
// would collide with the cron feature's meaning in a way a flat CLI command
// list can't disambiguate the way console's tabs do; "park" cannot be
// confused with either. This rename is CLI-surface only — it still calls the
// same ParkDeployment/project-scoped pause route as before (internal method
// name updated to match, the route itself is unchanged). Console ALSO has a
// second, newer path to the same user promise — a save-and-stop endpoint
// that fully closes the deployment and relies on a resumable close reason
// rather than a live paused lease — and whether this CLI verb should move
// onto that instead is a real, separate, still-open question this rename
// does not answer.
func park(args []string) error {
	fs := flag.NewFlagSet("park", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("a setup is required — usage: aq park <setup>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runPark(parkOptions{cred: cred, target: target, out: os.Stdout})
}

// runPark resolves the target to a setup id, finds the deployment holding
// its lease (a setup can only be parked while one does), and pauses that
// deployment via the project-scoped pause route. It never touches
// resolveDeploymentID: a setup's own id and the deployment id renting its
// compute are different objects in different id spaces, and park is the one
// verb here that legitimately needs both, in that order.
func runPark(opts parkOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	setup, err := findSetup(client, setupID)
	if err != nil {
		return err
	}
	if setup.LeaseDeploymentID == nil {
		return fmt.Errorf("%q is not running — nothing to park", setup.Name)
	}
	deploymentID := *setup.LeaseDeploymentID

	dep, err := client.GetDeployment(deploymentID)
	if err != nil {
		return fmt.Errorf("could not look up deployment #%d for %q: %w", deploymentID, setup.Name, err)
	}
	if dep.ProjectID == "" {
		return fmt.Errorf("deployment #%d for %q has no project on record — cannot park it", deploymentID, setup.Name)
	}

	if err := client.ParkDeployment(dep.ProjectID, deploymentID); err != nil {
		return fmt.Errorf("could not park %q: %w", setup.Name, err)
	}

	fmt.Fprintf(out, "✓ Saving %s and releasing the machine — resume it any time with \"aq up\".\n", setup.Name)
	return nil
}
