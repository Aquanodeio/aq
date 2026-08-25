package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// shareOptions configures runShare. share() fills in the real environment;
// tests call runShare directly.
type shareOptions struct {
	cred    *config.Credential
	target  string // setup id (uuid) or name
	version int    // the per-lineage version NUMBER to share (v1, v2, v3, ...)
	out     io.Writer
}

// share parses `aq share <setup> <version>` and wires the real environment
// into runShare.
//
// A share link addresses ONE immutable version of a setup's save lineage,
// never the lineage's current head — sharing v3 keeps pointing at v3's exact
// bytes even after v4, v5, ... get saved on top of it.
func share(args []string) error {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return fmt.Errorf("usage: aq share <setup> <version>")
	}
	target := positional[0]
	version, err := strconv.Atoi(positional[1])
	if err != nil || version <= 0 {
		return fmt.Errorf("invalid version %q — pass the version number shown by `aq save` or `aq setups` (e.g. 3 for v3)", positional[1])
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runShare(shareOptions{cred: cred, target: target, version: version, out: os.Stdout})
}

// runShare resolves the (setup, version-number) pair the user typed to the
// version's global row id, then mints and prints a share link for it.
func runShare(opts shareOptions) error {
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

	res, err := client.ShareSetupVersion(versionRowID)
	if err != nil {
		return fmt.Errorf("could not share version %d: %w", opts.version, err)
	}

	fmt.Fprintf(out, "%s\n", res.URL)
	return nil
}

// resolveSetupVersionRowID turns the (setup, version-NUMBER) pair a user
// types into the version table's global row id ShareSetupVersion needs.
//
// These are two different counters and must never be conflated: `version` is
// a per-lineage sequence that restarts at 1 for every lineage (comfyui's v1,
// v2, v3, ...), while the row id is the versions table's global
// autoincrement key. Treating the typed number as the id directly would mint
// a share link for whatever row happens to have that id — almost certainly a
// different setup, quite possibly a different account's data. So this
// always resolves through the API instead of ever guessing: list every
// version row the caller can see (ListAllSetupVersions — GET /setups has no
// nested "latest version"/lineage-name field to start a name-scoped lookup
// from, see internal/api/setups.go) and pick the one row whose SetupID
// matches AND whose Version matches what the user typed. No match is a hard
// error — this never falls back to treating the number as an id.
func resolveSetupVersionRowID(client *api.Client, setupID string, version int) (int, error) {
	setup, err := findSetup(client, setupID)
	if err != nil {
		return 0, err
	}

	versions, err := client.ListAllSetupVersions()
	if err != nil {
		return 0, fmt.Errorf("could not look up versions for %q: %w", setup.Name, err)
	}
	for _, v := range versions {
		if v.SetupID == setupID && v.Version == version {
			return v.ID, nil
		}
	}
	return 0, fmt.Errorf("no version %d found for %q — run `aq save` first", version, setup.Name)
}
