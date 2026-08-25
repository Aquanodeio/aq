package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// autopauseOptions configures runAutopause. autopause() fills in the real
// environment; tests call runAutopause directly.
type autopauseOptions struct {
	cred    *config.Credential
	target  string // setup id (uuid) or name
	enabled bool
	out     io.Writer
}

// autopause parses `aq autopause <setup> on|off` and wires the real
// environment into runAutopause.
func autopause(args []string) error {
	fs := flag.NewFlagSet("autopause", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" {
		return errors.New("usage: aq autopause <setup> on|off")
	}
	target := positional[0]

	var enabled bool
	switch positional[1] {
	case "on":
		enabled = true
	case "off":
		enabled = false
	default:
		return fmt.Errorf("aq autopause: expected \"on\" or \"off\", got %q", positional[1])
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runAutopause(autopauseOptions{cred: cred, target: target, enabled: enabled, out: os.Stdout})
}

// runAutopause sets the setup-level auto-pause PREFERENCE.
//
// "Pause" / "auto-pause" is the product's one settled noun for
// save-then-release (meta-repo ticket #753) — this command keeps the verb
// "autopause" (it predates the ticket and nothing else collides with it),
// but every string it prints uses the hyphenated "auto-pause" form to match
// console/docs/website.
//
// This is a different mechanism from `aq idle`, and the two never conflate:
// `aq idle set` writes a PER-DEPLOYMENT idle-threshold policy (warn/pause
// after minutes, GPU idle %) that always outranks whatever this sets — see
// idlePolicyFor in the orchestrator's idle.config.ts, which layers
// Setup.autopauseEnabled in underneath the deployment's own policy.
// Auto-pause carries no thresholds of its own: turning it on just means
// "pause this setup's box when it goes idle, using the platform's default
// thresholds." Use `aq idle set` to change WHEN idle counts as idle; use
// this to turn auto-pause on or off per setup.
func runAutopause(opts autopauseOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.SetSetupAutopause(setupID, opts.enabled)
	if err != nil {
		return fmt.Errorf("could not update auto-pause for setup %q: %w", opts.target, err)
	}

	// AutopauseEnabled is three-state on the wire (nil = unset, follows the
	// platform default). It should always come back non-nil right after this
	// call sets it explicitly, but render nil honestly rather than silently
	// treating it as "off" if the server ever surprises us.
	state := "unset (follows the platform default)"
	if res.AutopauseEnabled != nil {
		state = "off"
		if *res.AutopauseEnabled {
			state = "on"
		}
	}
	fmt.Fprintf(out, "✓ Auto-pause is now %s for %s.\n", state, res.Name)
	return nil
}
