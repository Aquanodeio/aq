package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// pauseOptions configures runPause. pause() fills in the real environment;
// tests call runPause directly.
type pauseOptions struct {
	cred   *config.Credential
	target string // setup id (uuid) or name
	out    io.Writer
}

// pause parses `aq pause <setup>` and wires the real environment into runPause.
//
// `aq pause` saves the setup's current state, then releases its rented box —
// the existing project pause route already snapshots before it lands the
// deployment PAUSED, so this is a thin CLI wrapper over that guarantee, not a
// second save path. Unlike `aq down`, pausing keeps the project's one
// resumable slot: a future `aq up`/`aq deploy` against the same setup picks
// it back up automatically.
//
// This command used to be named "park". Pause / auto-pause is the one noun
// the whole product (console, docs, website, aq) now uses for
// save-then-release, and resume is its opposite. `aq park` is kept below as
// a deprecated alias so a shipped script that still calls it does not break.
func pause(args []string) error {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("a setup is required — usage: aq pause <setup>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runPause(pauseOptions{cred: cred, target: target, out: os.Stdout})
}

// park is the deprecated alias for `aq pause` — see pause()'s doc comment.
func park(args []string) error {
	fmt.Fprintln(os.Stderr, `aq: "park" is deprecated, use "pause" instead`)
	fs := flag.NewFlagSet("park", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("a setup is required — usage: aq pause <setup>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runPause(pauseOptions{cred: cred, target: target, out: os.Stdout})
}

// runPause resolves the target to a setup id, finds the deployment holding
// its lease (a setup can only be paused while one does), and pauses that
// deployment via the project-scoped pause route. It never touches
// resolveDeploymentID: a setup's own id and the deployment id renting its
// compute are different objects in different id spaces, and pause is the one
// verb here that legitimately needs both, in that order.
func runPause(opts pauseOptions) error {
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
		return fmt.Errorf("%q is not running — nothing to pause", setup.Name)
	}
	deploymentID := *setup.LeaseDeploymentID

	dep, err := client.GetDeployment(deploymentID)
	if err != nil {
		return fmt.Errorf("could not look up deployment #%d for %q: %w", deploymentID, setup.Name, err)
	}
	if dep.ProjectID == "" {
		return fmt.Errorf("deployment #%d for %q has no project on record — cannot pause it", deploymentID, setup.Name)
	}

	if err := client.PauseDeployment(dep.ProjectID, deploymentID); err != nil {
		return fmt.Errorf("could not pause %q: %w", setup.Name, err)
	}

	fmt.Fprintf(out, "✓ Saving %s and releasing the machine — resume it any time with \"aq up\".\n", setup.Name)
	return nil
}
