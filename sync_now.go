package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// syncNowOptions configures runSyncNow. syncNow() fills in the real
// environment; tests call runSyncNow directly.
type syncNowOptions struct {
	cred   *config.Credential
	target string // setup id (uuid) or name
	out    io.Writer
}

// syncNow parses `aq sync-now <setup>` and wires the real environment into
// runSyncNow.
//
// Forces a sync tick right now, outside the setup's own scheduled interval —
// useful right before `aq share`/`aq fork` so a link points at your latest
// work instead of whatever the last scheduled tick happened to catch. The
// setup must currently be attached to a running deployment.
func syncNow(args []string) error {
	fs := flag.NewFlagSet("sync-now", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("a setup is required — usage: aq sync-now <setup>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runSyncNow(syncNowOptions{cred: cred, target: positional[0], out: os.Stdout})
}

// runSyncNow forces the sync tick and reports the resulting snapshot id.
func runSyncNow(opts syncNowOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.SyncSetupNow(setupID)
	if err != nil {
		return fmt.Errorf("could not sync %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Sync complete (snapshot %s)\n", res.SnapshotID)
	return nil
}
