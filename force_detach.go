package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// forceDetachOptions configures runForceDetach. forceDetach() fills in the
// real environment; tests call runForceDetach directly.
type forceDetachOptions struct {
	cred   *config.Credential
	target string // setup id (uuid) or name
	out    io.Writer
}

// forceDetach parses `aq force-detach <setup> --yes` and wires the real
// environment into runForceDetach.
//
// This breaks a setup's lease even while a sync is in flight. The
// orchestrator's own route (POST /setups/:id/force-detach) refuses without
// an explicit acknowledgeDataLoss:true, because force-breaking a lease
// mid-sync can lose anything written since the last COMPLETED sync. --yes is
// the CLI's mirror of that acknowledgement — there is no silent/default form
// of this call, and no other confirmation prompt substitutes for it.
func forceDetach(args []string) error {
	fs := flag.NewFlagSet("force-detach", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "acknowledge that work since the last completed sync may be lost")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("a setup is required — usage: aq force-detach <setup> --yes")
	}
	if !*yes {
		return errors.New("force-detach can lose work written since the last completed sync — rerun with --yes to confirm")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runForceDetach(forceDetachOptions{cred: cred, target: positional[0], out: os.Stdout})
}

// runForceDetach breaks the setup's lease and reports whether a sync was in
// flight when it did.
func runForceDetach(opts forceDetachOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.ForceDetachSetup(setupID)
	if err != nil {
		return fmt.Errorf("could not force-detach %q: %w", opts.target, err)
	}

	if res.WasSyncing {
		fmt.Fprintln(out, "✓ Lease force-broken while a sync was in progress — any data written since the last completed sync may be lost.")
		return nil
	}
	fmt.Fprintln(out, "✓ Lease force-broken.")
	return nil
}
