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

// env dispatches `aq env <sub>`, the Environment half of the
// pod/environment/volume model: everything OUTSIDE /workspace (base image,
// installed packages, startup script). It saves silently with its pod's
// config on every Stop and only becomes a visible, named object when Kept or
// Shared — there is no standalone "save environment" verb and no
// `/environments` browsing page; `keep`/`share`/`ls`/`rm` are the whole
// vocabulary.
func env(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq env <keep|share|ls|rm> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "keep":
		return envKeep(rest)
	case "share":
		return envShare(rest)
	case "ls":
		return envLs(rest)
	case "rm":
		return envRm(rest)
	default:
		return fmt.Errorf("aq env: unknown subcommand %q, expected one of keep, share, ls, rm", sub)
	}
}

// envKeepOptions configures runEnvKeep. envKeep() fills in the real
// environment; tests call runEnvKeep directly.
type envKeepOptions struct {
	cred   *config.Credential
	target string // pod id (uuid) or name
	name   string
	out    io.Writer
}

// envKeep parses `aq env keep <pod> <name>` and wires the real environment
// into runEnvKeep.
func envKeep(args []string) error {
	fs := flag.NewFlagSet("env keep", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq env keep <pod> <name>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runEnvKeep(envKeepOptions{cred: cred, target: positional[0], name: positional[1], out: os.Stdout})
}

// runEnvKeep names the pod's current environment. It mints nothing by
// itself — the pod's working environment (and any version it already has)
// is just filed under a name so it lists under Yours in the New pod picker
// and survives pod deletion.
func runEnvKeep(opts envKeepOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.KeepSetupEnvironment(setupID, opts.name)
	if err != nil {
		return fmt.Errorf("could not keep %q's environment: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Kept as %q (id %s). See it with `aq env ls`.\n", opts.name, res.EnvironmentID)
	return nil
}

// envShareOptions configures runEnvShare. envShare() fills in the real
// environment; tests call runEnvShare directly.
type envShareOptions struct {
	cred            *config.Credential
	target          string // pod id (uuid) or name; empty when fromEnvironment is set
	fromEnvironment string // an existing Kept/Shared environment's name or id
	versionID       string // optional for a pod target, required for fromEnvironment
	out             io.Writer
	pollInterval    time.Duration
	timeout         time.Duration
}

// envShare parses `aq env share <pod> [--version <id>]` (or
// `aq env share --from-environment <name|id> --version <id>` for an existing
// named environment, not necessarily on any pod right now) and wires the
// real environment into runEnvShare.
func envShare(args []string) error {
	fs := flag.NewFlagSet("env share", flag.ContinueOnError)
	version := fs.String("version", "", "Share this version instead of the latest (required with --from-environment)")
	fromEnv := fs.String("from-environment", "", "Share a version of an existing Kept/Shared environment directly, instead of a pod's own")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	opts := envShareOptions{versionID: *version, fromEnvironment: *fromEnv, out: os.Stdout}
	if *fromEnv != "" {
		if *version == "" {
			return errors.New("--version is required with --from-environment: an environment has no single pod to default the latest version from")
		}
		if len(positional) > 0 {
			return errors.New("pass a pod OR --from-environment, not both")
		}
	} else {
		if len(positional) == 0 || positional[0] == "" {
			return errors.New("usage: aq env share <pod> [--version <id>]\n   or: aq env share --from-environment <name|id> --version <id>")
		}
		opts.target = positional[0]
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}
	opts.cred = cred

	return runEnvShare(opts)
}

// runEnvShare shares one environment version and polls until the
// server-side publish job (a restic copy into a fresh per-version repo)
// reports ready or failed, printing "Preparing link..." while it waits — the
// link is not safe to hand out until GET /shares/:shareId says ready.
func runEnvShare(opts envShareOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = 2 * time.Second
	}
	if opts.timeout <= 0 {
		opts.timeout = 5 * time.Minute
	}

	client := newControlClient(opts.cred)

	var res *api.ShareResult
	var err error
	if opts.fromEnvironment != "" {
		var environmentID string
		environmentID, err = resolveEnvironmentID(client, opts.fromEnvironment)
		if err != nil {
			return err
		}
		res, err = client.ShareEnvironmentByID(environmentID, opts.versionID)
	} else {
		var setupID string
		setupID, err = resolveSetupID(client, opts.target)
		if err != nil {
			return err
		}
		printEnvironmentPreview(out, client, setupID)
		res, err = client.ShareSetupEnvironment(setupID, opts.versionID)
	}
	if err != nil {
		return fmt.Errorf("could not share the environment: %w", err)
	}

	if res.State == "ready" {
		fmt.Fprintf(out, "%s\n", res.ShareURL)
		return nil
	}

	fmt.Fprintln(out, "Preparing link...")
	if err := pollShareReady(client, res.ShareID, opts.pollInterval, opts.timeout); err != nil {
		return fmt.Errorf("share %s: %w", res.ShareID, err)
	}
	fmt.Fprintf(out, "%s\n", res.ShareURL)
	return nil
}

// printEnvironmentPreview shows the outgoing-paths preview before a
// pod-scoped share — included roots, what's left out, and any secret hits in
// the startup script — the same information the console's Share dialog shows
// before confirming. A failed preview lookup never blocks the share itself:
// it is a courtesy, not a gate (the server enforces the actual secret-hit
// refusal).
func printEnvironmentPreview(out io.Writer, client *api.Client, setupID string) {
	preview, err := client.GetEnvironmentPreview(setupID)
	if err != nil {
		return
	}
	fmt.Fprintln(out, "Going out:")
	for _, p := range preview.IncludedRoots {
		fmt.Fprintf(out, "  %s\n", p)
	}
	if len(preview.Excluded) > 0 {
		fmt.Fprintln(out, "Left out (private, never shared):")
		for _, p := range preview.Excluded {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
	if len(preview.SecretHits) > 0 {
		fmt.Fprintln(out, "⚠ The startup script looks like it contains a secret; the share will be refused:")
		for _, h := range preview.SecretHits {
			fmt.Fprintf(out, "  line %d: %s\n", h.Line, h.Kind)
		}
	}
}

// pollShareReady polls GET /shares/:shareId until it reports ready or
// failed, or timeout elapses.
func pollShareReady(client *api.Client, shareID string, pollInterval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := client.GetShareStatus(shareID)
		if err != nil {
			return fmt.Errorf("could not check share status: %w", err)
		}
		switch status.State {
		case "ready":
			return nil
		case "failed":
			reason := "no reason given"
			if status.Error != nil && *status.Error != "" {
				reason = *status.Error
			}
			return fmt.Errorf("preparing the link failed: %s", reason)
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the link to be ready; check it later with the shareId already printed above")
		}
		time.Sleep(pollInterval)
	}
}

// envLsOptions configures runEnvLs. envLs() fills in the real environment;
// tests call runEnvLs directly.
type envLsOptions struct {
	cred   *config.Credential
	target string // optional: a pod or environment name/id to list versions of
	out    io.Writer
}

// envLs parses `aq env ls [<pod-or-environment>]` and wires the real
// environment into runEnvLs.
func envLs(args []string) error {
	fs := flag.NewFlagSet("env ls", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	opts := envLsOptions{cred: cred, out: os.Stdout}
	if len(positional) > 0 {
		opts.target = positional[0]
	}
	return runEnvLs(opts)
}

// runEnvLs lists every environment the caller can pick from (no target), or
// one pod's/environment's version history (target given). A target is tried
// as a pod first, then as a named environment — read-only, so a wrong guess
// costs nothing.
func runEnvLs(opts envLsOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	client := newControlClient(opts.cred)

	if opts.target == "" {
		envs, err := client.ListEnvironments()
		if err != nil {
			return fmt.Errorf("could not list environments: %w", err)
		}
		printEnvironmentGroups(out, envs)
		return nil
	}

	if setupID, err := resolveSetupID(client, opts.target); err == nil {
		versions, err := client.ListSetupEnvironmentVersions(setupID)
		if err != nil {
			return fmt.Errorf("could not list versions for %q: %w", opts.target, err)
		}
		printEnvironmentVersions(out, versions)
		return nil
	}

	environmentID, err := resolveEnvironmentID(client, opts.target)
	if err != nil {
		return fmt.Errorf("no pod or environment named %q", opts.target)
	}
	versions, err := client.ListEnvironmentVersions(environmentID)
	if err != nil {
		return fmt.Errorf("could not list versions for %q: %w", opts.target, err)
	}
	printEnvironmentVersions(out, versions)
	return nil
}

// printEnvironmentGroups renders GET /environments' three picker groups.
func printEnvironmentGroups(out io.Writer, envs *api.EnvironmentsResult) {
	printEnvironmentGroup(out, "Built-in", envs.Builtin)
	printEnvironmentGroup(out, "Yours", envs.Yours)
	printEnvironmentGroup(out, "Shared with you", envs.Shared)
}

func printEnvironmentGroup(out io.Writer, label string, list []api.EnvironmentSummary) {
	fmt.Fprintf(out, "%s:\n", label)
	if len(list) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}
	for _, e := range list {
		fmt.Fprintf(out, "  %-24s  %s\n", truncate(e.Name, 24), e.ID)
	}
}

// printEnvironmentVersions renders a version-history table shared by
// `aq env ls <pod>` and `aq env ls <environment>`.
func printEnvironmentVersions(out io.Writer, versions []api.EnvironmentVersion) {
	if len(versions) == 0 {
		fmt.Fprintln(out, "No versions yet. `aq env keep`/`aq env share` mints the first one.")
		return
	}
	fmt.Fprintf(out, "%-36s  %-4s  %-24s  %-9s  %s\n", "ID", "VER", "CREATED", "INCLUDES", "EXCLUDES")
	for _, v := range versions {
		fmt.Fprintf(out, "%-36s  v%-3d  %-24s  %-9d  %d\n", v.ID, v.Version, orDash(v.CreatedAt), len(v.IncludedRoots), len(v.Excluded))
	}
}

// envRmOptions configures runEnvRm. envRm() fills in the real environment;
// tests call runEnvRm directly.
type envRmOptions struct {
	cred   *config.Credential
	target string // environment name or id
	out    io.Writer
}

// envRm parses `aq env rm <name|id>` and wires the real environment into
// runEnvRm.
func envRm(args []string) error {
	fs := flag.NewFlagSet("env rm", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq env rm <name|id>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runEnvRm(envRmOptions{cred: cred, target: positional[0], out: os.Stdout})
}

// runEnvRm deletes a Kept or Shared environment. Breaks nothing running;
// existing share links to it stop working.
func runEnvRm(opts envRmOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	environmentID, err := resolveEnvironmentID(client, opts.target)
	if err != nil {
		return err
	}

	if err := client.DeleteEnvironment(environmentID); err != nil {
		return fmt.Errorf("could not delete environment %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Deleted environment %q.\n", opts.target)
	return nil
}
