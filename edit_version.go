package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// editVersionOptions configures runEditVersion. editVersion() fills in the
// real environment; tests call runEditVersion directly.
type editVersionOptions struct {
	cred        *config.Credential
	target      string // setup id (uuid) or name
	version     int    // the per-lineage version NUMBER (v1, v2, v3, ...)
	label       *string
	description *string
	visibility  string
	out         io.Writer
}

// editVersion parses `aq edit-version <setup> <version> [flags]` and wires
// the real environment into runEditVersion.
//
// Edits a saved version's label, description, and/or visibility
// (private/team/public) — the same three fields the console's version
// settings sheet edits, and the only three the server accepts (the request
// is `.strict()`). --label/--description only ever SET a value here; there
// is currently no flag to clear one back to empty.
func editVersion(args []string) error {
	fs := flag.NewFlagSet("edit-version", flag.ContinueOnError)
	label := fs.String("label", "", "set the version's label")
	description := fs.String("description", "", "set the version's description")
	visibility := fs.String("visibility", "", "set visibility: private, team, or public")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq edit-version <setup> <version> [--label ...] [--description ...] [--visibility private|team|public]")
	}
	target := positional[0]
	version, err := strconv.Atoi(positional[1])
	if err != nil || version <= 0 {
		return fmt.Errorf("invalid version %q — pass the version number shown by `aq save`/`aq setups` (e.g. 3 for v3)", positional[1])
	}

	if *visibility != "" && *visibility != "private" && *visibility != "team" && *visibility != "public" {
		return fmt.Errorf("--visibility must be private, team, or public, got %q", *visibility)
	}

	opts := editVersionOptions{target: target, version: version, visibility: *visibility}
	if *label != "" {
		opts.label = label
	}
	if *description != "" {
		opts.description = description
	}
	if opts.label == nil && opts.description == nil && opts.visibility == "" {
		return errors.New("nothing to update — pass at least one of --label, --description, --visibility")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}
	opts.cred = cred
	opts.out = os.Stdout

	return runEditVersion(opts)
}

// runEditVersion resolves the (setup, version-number) pair to the version's
// global row id (same resolveSetupVersionRowID `aq share` uses) and applies
// the update.
func runEditVersion(opts editVersionOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}
	versionRowID, err := resolveSetupVersionRowID(client, setupID, opts.version)
	if err != nil {
		return err
	}

	res, err := client.UpdateSnapshotVersion(versionRowID, api.UpdateSnapshotVersionRequest{
		Label:       opts.label,
		Description: opts.description,
		Visibility:  opts.visibility,
	})
	if err != nil {
		return fmt.Errorf("could not update version %d: %w", opts.version, err)
	}

	fmt.Fprintf(out, "✓ Updated %s v%d\n", res.Name, res.Version)
	return nil
}
