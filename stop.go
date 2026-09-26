package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// stopOptions configures runStop. stop() fills in the real environment;
// tests call runStop directly.
type stopOptions struct {
	cred   *config.Credential
	target string // pod id (uuid) or name
	out    io.Writer
}

// stop parses `aq stop <pod>` and wires the real environment into runStop.
//
// Stop saves the pod's environment and volume (both confirmed) and then
// releases its box — always, with no flag to skip it: this is the one thing
// that changed most in the pod/environment/volume model. The pod keeps its
// config and full history; bring it back with `aq start`. There is no more
// separate "pause" verb, and no "resume" — Stop/Start replace both, and
// unlike the old pause/resume pair, Start never has to target the SAME
// machine or a specific deployment id.
func stop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("a pod is required, usage: aq stop <pod>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runStop(stopOptions{cred: cred, target: positional[0], out: os.Stdout})
}

// runStop resolves the target to a pod id and stops it.
func runStop(opts stopOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.StopSetup(setupID)
	if err != nil {
		return fmt.Errorf("could not stop %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Saved and stopped %s.\n", res.Name)
	printPodStorageSummary(out, *res)
	fmt.Fprintf(out, "\nStart it again any time with:\n  aq start %s\n", res.Name)
	return nil
}
