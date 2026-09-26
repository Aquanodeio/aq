package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// autostopOptions configures runAutostop. autostop() fills in the real
// environment; tests call runAutostop directly.
type autostopOptions struct {
	cred    *config.Credential
	target  string // pod id (uuid) or name
	enabled bool
	out     io.Writer
}

// autostop parses `aq autostop <pod> on|off` and wires the real environment
// into runAutostop. Replaces `aq autopause`: same mechanism, renamed per the
// pod/environment/volume plan's vocabulary sweep (PUT /setups/:id/autostop
// replaces PUT /setups/:id/autopause; "pause" is retired everywhere except
// environment versions).
func autostop(args []string) error {
	fs := flag.NewFlagSet("autostop", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" {
		return errors.New("usage: aq autostop <pod> on|off")
	}
	target := positional[0]

	var enabled bool
	switch positional[1] {
	case "on":
		enabled = true
	case "off":
		enabled = false
	default:
		return fmt.Errorf("aq autostop: expected \"on\" or \"off\", got %q", positional[1])
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runAutostop(autostopOptions{cred: cred, target: target, enabled: enabled, out: os.Stdout})
}

// runAutostop sets the pod-level auto-stop PREFERENCE.
//
// This is a different mechanism from `aq idle`, and the two never conflate:
// `aq idle set` writes a PER-DEPLOYMENT idle-threshold policy (warn/stop
// after minutes, GPU idle %) that always outranks whatever this sets. Auto-
// stop carries no thresholds of its own: turning it on just means "stop this
// pod's box when it goes idle, using the platform's default thresholds." Use
// `aq idle set` to change WHEN idle counts as idle; use this to turn
// auto-stop on or off per pod.
func runAutostop(opts autostopOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.SetSetupAutostop(setupID, opts.enabled)
	if err != nil {
		return fmt.Errorf("could not update auto-stop for pod %q: %w", opts.target, err)
	}

	// AutostopEnabled is three-state on the wire (nil = unset, follows the
	// platform default). It should always come back non-nil right after this
	// call sets it explicitly, but render nil honestly rather than silently
	// treating it as "off" if the server ever surprises us.
	state := "unset (follows the platform default)"
	if res.AutostopEnabled != nil {
		state = "off"
		if *res.AutostopEnabled {
			state = "on"
		}
	}
	fmt.Fprintf(out, "✓ Auto-stop is now %s for %s.\n", state, res.Name)
	return nil
}
