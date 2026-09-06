package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
)

// pods parses `aq pods` and wires the real environment into runPods.
//
// `aq pods` lists what the caller owns, independent of whether a pod's
// compute is currently rented — name, running/not, latest saved version, and
// size on disk.
func pods(args []string) error {
	fs := flag.NewFlagSet("pods", flag.ContinueOnError)
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
		return fmt.Errorf("could not list pods: %w", err)
	}

	// GET /setups carries no nested "latest version" per row (see the Setup
	// doc comment in internal/api/setups.go) — recover it from the one call
	// that lists every version the caller can see, rather than one lookup
	// per pod. A failure here degrades the VERSION column to "-" instead
	// of failing the whole list; the pods themselves are already in hand.
	versions, err := client.ListAllSetupVersions()
	if err != nil {
		versions = nil
	}

	printPods(os.Stdout, list, latestVersionsByPod(versions))
	return nil
}

// latestVersionsByPod reduces a flat version list (as returned by
// ListAllSetupVersions) to each pod's highest Version number, keyed by
// SetupID. Legacy/external rows with no SetupID are naturally excluded —
// the zero value never matches a real pod id.
func latestVersionsByPod(versions []api.SetupVersion) map[string]int {
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

// printPods renders the pod list as a simple aligned table, or a
// one-line nudge when the caller owns none yet. latest maps pod id to its
// highest saved version number (see latestVersionsByPod); a pod absent
// from it renders "-".
func printPods(out io.Writer, list []api.Setup, latest map[string]int) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No pods yet. Run `aq up` to start one.")
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
		fmt.Fprintf(out, "%-24s  %-7s  %-7s  %s\n", s.Name, running, version, formatPodSize(int64(s.SizeBytes)))
	}
}

// formatPodSize renders a byte count in the largest whole binary unit that
// keeps it readable, at GiB precision — matching how held-snapshot storage
// is billed (see heldStorageRateLabel in pricing.go).
func formatPodSize(n int64) string {
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
