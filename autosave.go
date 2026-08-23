package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// autosaveOptions configures runAutosave. autosave() fills in the real
// environment; tests call runAutosave directly.
type autosaveOptions struct {
	cred    *config.Credential
	target  string // setup id (uuid) or name
	enabled bool
	out     io.Writer
}

// autosave parses `aq autosave <setup> on|off` and wires the real environment
// into runAutosave.
func autosave(args []string) error {
	fs := flag.NewFlagSet("autosave", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" {
		return errors.New("usage: aq autosave <setup> on|off")
	}
	target := positional[0]

	var enabled bool
	switch positional[1] {
	case "on":
		enabled = true
	case "off":
		enabled = false
	default:
		return fmt.Errorf("aq autosave: expected \"on\" or \"off\", got %q", positional[1])
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runAutosave(autosaveOptions{cred: cred, target: target, enabled: enabled, out: os.Stdout})
}

// runAutosave turns a setup's autosave on or off.
//
// Autosave keeps ONE always-current copy of the setup, not a history — it is
// NOT undo. Deleting your own work is replicated into that copy on the next
// tick just like any other change, so it guards against a killed provider,
// never against yourself. Turning it on also starts billing storage for the
// held snapshot, so both facts are printed before the API call that flips it
// on — never buried after, and never left for the user to discover on their
// first invoice.
func runAutosave(opts autosaveOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	if opts.enabled {
		fmt.Fprintln(out, "Autosave keeps ONE always-current copy of this setup — it is not undo.")
		fmt.Fprintln(out, "If you delete your own work, that gets replicated into the copy on the next tick too.")
		fmt.Fprintf(out, "Held snapshot storage is billed at %s.\n", heldStorageRateLabel)
	}

	res, err := client.SetSetupAutosave(setupID, opts.enabled)
	if err != nil {
		return fmt.Errorf("could not update autosave for setup %q: %w", opts.target, err)
	}

	state := "off"
	if res.AutosaveEnabled {
		state = "on"
	}
	fmt.Fprintf(out, "✓ Autosave is now %s for %s.\n", state, res.Name)
	return nil
}
