package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
)

// setups parses `aq setups` and wires the real environment into runSetups.
//
// `aq setups` lists what the caller owns, independent of whether a setup's
// compute is currently rented — name, running/not, latest saved version, and
// size on disk.
func setups(args []string) error {
	fs := flag.NewFlagSet("setups", flag.ContinueOnError)
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	client := newControlClient(cred)
	list, err := client.ListSetups()
	if err != nil {
		return fmt.Errorf("could not list setups: %w", err)
	}

	// GET /setups carries no nested "latest version" per row (see the Setup
	// doc comment in internal/api/setups.go) — recover it from the one call
	// that lists every version the caller can see, rather than one lookup
	// per setup. A failure here degrades the VERSION column to "-" instead
	// of failing the whole list; the setups themselves are already in hand.
	versions, err := client.ListAllSetupVersions()
	if err != nil {
		versions = nil
	}

	printSetups(os.Stdout, list, latestVersionsBySetup(versions))
	return nil
}

// latestVersionsBySetup reduces a flat version list (as returned by
// ListAllSetupVersions) to each setup's highest Version number, keyed by
// SetupID. Legacy/external rows with no SetupID are naturally excluded —
// the zero value never matches a real setup id.
func latestVersionsBySetup(versions []api.SetupVersion) map[string]int {
	m := make(map[string]int)
	for _, v := range versions {
		if v.SetupID == "" {
			continue
		}
		if v.Version > m[v.SetupID] {
			m[v.SetupID] = v.Version
		}
	}
	return m
}

// printSetups renders the setup list as a simple aligned table, or a
// one-line nudge when the caller owns none yet. latest maps setup id to its
// highest saved version number (see latestVersionsBySetup); a setup absent
// from it renders "-".
func printSetups(out io.Writer, list []api.Setup, latest map[string]int) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No setups yet. Run `aq up` to start one.")
		return
	}

	fmt.Fprintf(out, "%-24s  %-7s  %-7s  %s\n", "NAME", "RUNNING", "VERSION", "SIZE")
	for _, s := range list {
		running := "no"
		if s.Running() {
			running = "yes"
		}
		version := "-"
		if v, ok := latest[s.ID]; ok {
			version = fmt.Sprintf("v%d", v)
		}
		fmt.Fprintf(out, "%-24s  %-7s  %-7s  %s\n", s.Name, running, version, formatSetupSize(int64(s.SizeBytes)))
	}
}

// formatSetupSize renders a byte count in the largest whole binary unit that
// keeps it readable, at GiB precision — matching how held-snapshot storage
// is billed (see heldStorageRateLabel in pricing.go).
func formatSetupSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
