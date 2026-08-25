package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// idle dispatches `aq idle status|set` — the idle-auto-pause policy commands.
//
// This is the CLI-side half of a policy the console already exposes: a
// setting only reachable from the browser is invisible to anyone who deploys
// or drives their boxes from a terminal or a script, which is exactly how the
// idle policy's own effect (a box quietly paused) would otherwise get
// noticed too late.
func idle(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq idle <status|set> <deploymentId> [flags]")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		return idleStatus(rest)
	case "set":
		return idleSet(rest)
	default:
		return fmt.Errorf("aq idle: unknown subcommand %q — expected \"status\" or \"set\"", sub)
	}
}

// idleStatusOptions configures runIdleStatus. idleStatus() fills in the real
// environment; tests inject a base URL and a buffer writer.
type idleStatusOptions struct {
	cred   *config.Credential
	target string
	out    io.Writer
}

// idleStatus parses `aq idle status <deploymentId>` and wires the real
// environment into runIdleStatus.
func idleStatus(args []string) error {
	fs := flag.NewFlagSet("idle status", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	target, err := parseDeploymentTarget(positional, "idle status")
	if err != nil {
		return err
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return runIdleStatus(idleStatusOptions{cred: cred, target: target, out: os.Stdout})
}

// runIdleStatus fetches and renders a deployment's idle-auto-pause policy.
func runIdleStatus(opts idleStatusOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	deploymentID, err := resolveDeploymentID(client, opts.target, "idle status")
	if err != nil {
		return err
	}

	policy, err := client.GetIdlePolicy(deploymentID)
	if err != nil {
		return fmt.Errorf("could not fetch idle policy for deployment #%d: %w", deploymentID, err)
	}

	printIdlePolicy(out, *policy)
	return nil
}

// printIdlePolicy renders a merged idle policy + live verdict.
//
// UNKNOWN must render as unknown — never as ACTIVE and never as IDLE. UNKNOWN
// means the box has reported no usable usage data, and presenting that as
// either extreme is a lie the user could act on (stop a box that's actually
// busy, or trust auto-pause on a box it can't currently see).
func printIdlePolicy(out io.Writer, p api.IdlePolicy) {
	fmt.Fprintf(out, "state       %s\n", idleStateLabel(p.State, p.IdleMinutes))
	printIdlePolicySettings(out, p)
}

// printIdlePolicySettings renders the policy fields that both GET and PUT
// idle-policy responses carry: warn/stop thresholds, enabled flag, GPU idle
// threshold. It deliberately excludes the live state/idleMinutes verdict —
// see printIdlePolicy for that, and runIdleSet for why the write path never
// has one to print.
func printIdlePolicySettings(out io.Writer, p api.IdlePolicy) {
	fmt.Fprintf(out, "warn after  %s\n", formatMinutes(p.WarnAfterMinutes))
	enabled := "disabled"
	if p.AutoPauseEnabled {
		enabled = "enabled"
	}
	fmt.Fprintf(out, "pause after  %s  (%s)\n", formatMinutes(p.ActAfterMinutes), enabled)
	fmt.Fprintf(out, "gpu idle threshold  %d%%\n", p.GPUIdleThresholdPercent)
}

// idleStateLabel renders the live verdict honestly. UNKNOWN is its own label,
// distinct from both ACTIVE and IDLE.
func idleStateLabel(state string, idleMinutes int) string {
	switch state {
	case "ACTIVE":
		return "ACTIVE"
	case "IDLE_WARN":
		return fmt.Sprintf("IDLE, warned (%d min)", idleMinutes)
	case "IDLE_ACT":
		return fmt.Sprintf("IDLE, pausing (%d min)", idleMinutes)
	case "UNKNOWN":
		return "UNKNOWN — no usage data yet"
	default:
		// A future server-side state we don't know about — show it verbatim
		// rather than mapping it into one of the above and risking a wrong claim.
		return state
	}
}

// formatMinutes renders a minute count the way a user would type it back as a
// --warn-after/--stop-after duration flag (e.g. "30m", "1h", "1h30m").
func formatMinutes(m int) string {
	if m <= 0 {
		return fmt.Sprintf("%dm", m)
	}
	h, rem := m/60, m%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", rem)
	case rem == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%dm", h, rem)
	}
}

// idleSetOptions configures runIdleSet. Each field is nil unless the user
// actually passed the corresponding flag — a nil field must never be sent to
// the API, or an unset flag would silently overwrite the user's other
// settings with a zero value.
type idleSetOptions struct {
	cred             *config.Credential
	target           string
	warnAfter        *int // minutes
	pauseAfter       *int // minutes
	gpuThreshold     *int // percent
	autoPauseEnabled *bool
	out              io.Writer
}

// parseIdleSetArgs parses `aq idle set`'s flags and positional target,
// letting them appear in any order (parseInterspersed).
func parseIdleSetArgs(args []string) (idleSetOptions, error) {
	fs := flag.NewFlagSet("idle set", flag.ContinueOnError)
	warnAfterStr := fs.String("warn-after", "", "warn after this much idle time, e.g. 30m, 1h")
	pauseAfterStr := fs.String("pause-after", "", "auto-pause after this much idle time, e.g. 1h")
	// stopAfterStr is the deprecated pre-#753 name for --pause-after, kept
	// working (undocumented) so a shipped script doesn't break.
	stopAfterStr := fs.String("stop-after", "", "")
	gpuThreshold := fs.Int("gpu-threshold", -1, "GPU utilization percent below which the box counts as idle (0-100)")
	on := fs.Bool("on", false, "enable idle auto-pause")
	off := fs.Bool("off", false, "disable idle auto-pause")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return idleSetOptions{}, err
	}

	if *on && *off {
		return idleSetOptions{}, errors.New("pass at most one of --on / --off")
	}

	if *pauseAfterStr != "" && *stopAfterStr != "" {
		return idleSetOptions{}, errors.New("pass at most one of --pause-after / --stop-after (the latter is deprecated)")
	}
	effectivePauseAfterStr := *pauseAfterStr
	if effectivePauseAfterStr == "" && *stopAfterStr != "" {
		fmt.Fprintln(os.Stderr, `aq: "--stop-after" is deprecated, use "--pause-after" instead`)
		effectivePauseAfterStr = *stopAfterStr
	}

	var opts idleSetOptions
	if len(positional) > 0 {
		opts.target = positional[0]
	}

	if *warnAfterStr != "" {
		m, err := parsePositiveMinutes("--warn-after", *warnAfterStr)
		if err != nil {
			return idleSetOptions{}, err
		}
		opts.warnAfter = &m
	}
	if effectivePauseAfterStr != "" {
		m, err := parsePositiveMinutes("--pause-after", effectivePauseAfterStr)
		if err != nil {
			return idleSetOptions{}, err
		}
		opts.pauseAfter = &m
	}
	if *gpuThreshold != -1 {
		if *gpuThreshold < 0 || *gpuThreshold > 100 {
			return idleSetOptions{}, fmt.Errorf("--gpu-threshold must be 0-100, got %d", *gpuThreshold)
		}
		opts.gpuThreshold = gpuThreshold
	}
	if *on {
		t := true
		opts.autoPauseEnabled = &t
	}
	if *off {
		f := false
		opts.autoPauseEnabled = &f
	}

	// Client-side mirror of the server's rule, so a doomed request fails fast
	// with a clear message instead of a round trip for a 400. The server
	// remains the real authority — it also catches the case where only one of
	// the two is being changed against the deployment's existing other value.
	if opts.warnAfter != nil && opts.pauseAfter != nil && *opts.warnAfter >= *opts.pauseAfter {
		return idleSetOptions{}, fmt.Errorf(
			"--warn-after (%s) must be less than --pause-after (%s)",
			formatMinutes(*opts.warnAfter), formatMinutes(*opts.pauseAfter),
		)
	}

	if opts.warnAfter == nil && opts.pauseAfter == nil && opts.gpuThreshold == nil && opts.autoPauseEnabled == nil {
		return idleSetOptions{}, errors.New("nothing to update — pass at least one of --warn-after, --pause-after, --gpu-threshold, --on/--off")
	}

	return opts, nil
}

// parsePositiveMinutes parses a Go-style duration flag (e.g. "30m", "1h") and
// converts it to whole minutes, the unit the API speaks.
func parsePositiveMinutes(flagName, raw string) (int, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w (try a Go-style duration like 30m or 1h)", flagName, raw, err)
	}
	m := int(d.Minutes())
	if m <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", flagName, raw)
	}
	return m, nil
}

// idleSet parses flags/the deployment target and wires the real environment
// into runIdleSet.
//
// `aq idle set <deploymentId> [flags]` updates a deployment's idle-auto-pause
// policy — the same policy the console's deployment settings edit.
func idleSet(args []string) error {
	opts, err := parseIdleSetArgs(args)
	if err != nil {
		return err
	}
	if opts.target == "" {
		return errors.New("a deployment id is required — usage: aq idle set <deploymentId> [flags]")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}
	opts.cred = cred
	opts.out = os.Stdout

	return runIdleSet(opts)
}

// runIdleSet applies the update and prints the resulting settings.
//
// It prints only the policy fields (warn/stop/threshold/enabled), never a
// "state" line: PUT /idle-policy never computes a live idle verdict (that
// requires the last several hours of usage samples, which the write path
// doesn't fetch) — the field is genuinely absent from the response, not just
// unrendered. `aq idle status` is the command that reports live state.
func runIdleSet(opts idleSetOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	deploymentID, err := resolveDeploymentID(client, opts.target, "idle set")
	if err != nil {
		return err
	}

	policy, err := client.SetIdlePolicy(deploymentID, api.IdlePolicyUpdate{
		WarnAfterMinutes:        opts.warnAfter,
		ActAfterMinutes:         opts.pauseAfter,
		GPUIdleThresholdPercent: opts.gpuThreshold,
		AutoPauseEnabled:        opts.autoPauseEnabled,
	})
	if err != nil {
		return fmt.Errorf("could not update idle policy for deployment #%d: %w", deploymentID, err)
	}

	fmt.Fprintf(out, "✓ Updated idle policy for deployment #%d\n\n", deploymentID)
	printIdlePolicySettings(out, *policy)
	return nil
}
