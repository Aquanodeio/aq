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

	printSetups(os.Stdout, list)
	return nil
}

// printSetups renders the setup list as a simple aligned table, or a
// one-line nudge when the caller owns none yet.
func printSetups(out io.Writer, list []api.Setup) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No setups yet — run `aq up` to start one.")
		return
	}

	fmt.Fprintf(out, "%-24s  %-7s  %-7s  %s\n", "NAME", "RUNNING", "VERSION", "SIZE")
	for _, s := range list {
		running := "no"
		if s.Running() {
			running = "yes"
		}
		version := "-"
		if s.LatestVersion != nil {
			version = fmt.Sprintf("v%d", s.LatestVersion.Version)
		}
		fmt.Fprintf(out, "%-24s  %-7s  %-7s  %s\n", s.Name, running, version, formatSetupSize(s.SizeBytes))
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
