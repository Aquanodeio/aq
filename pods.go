package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
)

// pods parses `aq pods` and wires the real environment into runPods.
// `aq pods create` is a subcommand (see pods_create.go); every other form
// of `aq pods` lists what the caller owns, independent of whether a pod's
// compute is currently rented: name, running/not, current environment, and
// size on disk.
func pods(args []string) error {
	if len(args) > 0 && args[0] == "create" {
		return podsCreate(args[1:])
	}
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

	printPods(os.Stdout, list)
	return nil
}

// printPods renders the pod list as a simple aligned table, or a
// one-line nudge when the caller owns none yet.
func printPods(out io.Writer, list []api.Setup) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No pods yet. Run `aq up` to start one.")
		return
	}

	fmt.Fprintf(out, "%-24s  %-8s  %-24s  %s\n", "NAME", "RUNNING", "ENVIRONMENT", "SIZE")
	for _, s := range list {
		running := "no"
		switch {
		case s.Stopping:
			// A Stop, or the first half of a Move, is in flight: neither
			// cleanly Running nor cleanly Stopped yet.
			running = "stopping"
		case s.Running():
			running = "yes"
		}
		// A Setup carries no whole-pod size of its own any more (D8: the
		// environment and the volume each have their own); show the
		// volume's, the only size a pod row can honestly claim. A bare pod
		// (D4, no volume attached) and an attached-but-unmeasured volume are
		// two different facts, matching the console's own PodCard: "nothing
		// held" vs "not measured yet", never the same dash for both.
		size := "nothing held"
		if s.Volume != nil {
			size = formatPodSizePtr(s.Volume.SizeBytes)
		}
		fmt.Fprintf(out, "%-24s  %-8s  %-24s  %s\n", s.Name, running, formatPodEnvironment(s.Environment), size)
	}
}

// formatPodEnvironment renders a pod's current environment as "name vN", or
// bare "name" when it has no minted version yet: Version is nullable on the
// wire and stays null until the pod's environment is Kept or Shared for the
// first time (SetupEnvironmentSummary's doc comment in internal/api/setups.go).
// A nil environment is the rare case where the backend's environmentVersionId
// didn't resolve (same doc comment): it renders as "unknown", never a bare
// "-" indistinguishable from a normal, if plain, environment name.
func formatPodEnvironment(e *api.SetupEnvironmentSummary) string {
	if e == nil {
		return "unknown"
	}
	if e.Version == nil {
		return orDash(e.Name)
	}
	return fmt.Sprintf("%s v%d", e.Name, *e.Version)
}

// printPodStorageSummary renders a pod's Environment/Volume state after
// Start/Stop/Move, matching the console pod-detail line: environment name
// (plus its version once one is minted, same "name vN" shape `aq pods`
// uses), volume name, size, and its three-state save status. Volume is nil
// for a pod running with no volume attached (D4: a bare pod is allowed),
// printed as nothing, never a blank/zeroed row.
func printPodStorageSummary(out io.Writer, s api.Setup) {
	fmt.Fprintf(out, "  Environment: %s\n", formatPodEnvironment(s.Environment))
	if s.Volume == nil {
		return
	}
	v := *s.Volume
	fmt.Fprintf(out, "  Volume: %s (%s, %s)\n", orDash(v.Name), formatPodSizePtr(v.SizeBytes), saveStateLabel(v.SaveState))
}

// formatPodSize renders a byte count in the largest whole binary unit that
// keeps it readable, at GiB precision, matching how held-snapshot storage
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

// formatPodSizePtr renders a nullable byte count (SetupVolumeSummary's and
// Volume's SizeBytes are both null before storage metering has ever
// measured that volume). A nil pointer renders as "not measured yet",
// matching the console's own wording (PodCard.tsx), never "0 B" or a bare
// "-": all three mean different things (unmeasured, a real empty volume, and
// no data at all).
func formatPodSizePtr(n *int64) string {
	if n == nil {
		return "not measured yet"
	}
	return formatPodSize(*n)
}
