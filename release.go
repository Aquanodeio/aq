package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// releaseOptions configures runRelease.
type releaseOptions struct {
	alias  string
	yes    bool
	keep   bool
	out    io.Writer
	errOut io.Writer

	client *api.Client
}

// releaseCmd parses `aq release <alias>`.
//
// The verb is `release`, and it is not a synonym for anything else in this CLI.
// It is NOT "terminate": nothing is torn down, no provider is contacted, and the
// box keeps running on the lease its owner is already paying for. It is NOT
// "detach" either — `aq force-detach` breaks a setup's single-writer lease (and
// can lose work since the last completed sync) and `aq run --detach` is a
// background run. Both of those meanings are live, so this one gets its own
// word rather than a third overload of a taken one.
func releaseCmd(args []string) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "Skip the interactive confirmation")
	keep := fs.Bool("keep-host", false, "Keep the box in your local host registry (it stays usable in detached mode)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: aq release <alias>: the alias of an attached box from `aq host ls`")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runRelease(releaseOptions{
		alias:  positional[0],
		yes:    *yes,
		keep:   *keep,
		out:    os.Stdout,
		errOut: os.Stderr,
		client: newControlClient(cred),
	})
}

// runRelease revokes the box's credentials and drops its deployment row.
func runRelease(opts releaseOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}

	h, err := lookupHost(opts.alias)
	if err != nil {
		return err
	}
	if !h.Releasable() {
		return fmt.Errorf("%s is not attached; there is nothing to release. Remove it from your registry with `aq host rm %s`", h.Alias, h.Alias)
	}
	deploymentID := h.ReleaseTargetID()

	if h.Attached() {
		fmt.Fprintf(opts.out, "Release %s (deployment #%d):\n", h.Alias, deploymentID)
	} else {
		// A row `aq attach` registered but never finished activating — no
		// credentials were ever written to reach this box, so there is nothing
		// live to revoke, only a PROVISIONING row to drop server-side.
		fmt.Fprintf(opts.out, "Release %s (deployment #%d, never finished attaching):\n", h.Alias, deploymentID)
	}
	fmt.Fprintln(opts.out, "  • Aquanode drops this box's deployment row")
	fmt.Fprintln(opts.out, "  • the box keeps running, exactly as it is")
	fmt.Fprintln(opts.out, "  • no provider is contacted and nothing is torn down. This is not a terminate")
	if h.Attached() {
		fmt.Fprintln(opts.out, "  • Aquanode revokes this box's credentials")
		fmt.Fprintln(opts.out, "  • the console, version history, sharing and jobs stop working for it")
	}
	if opts.keep {
		fmt.Fprintf(opts.out, "  • %s stays in your local registry and keeps working in detached mode\n", h.Alias)
	} else {
		fmt.Fprintf(opts.out, "  • %s is removed from your local registry (--keep-host keeps it for detached use)\n", h.Alias)
	}

	if !opts.yes {
		if !isInteractiveStdin() {
			return errors.New("refusing to release without confirmation in a non-interactive shell; re-run with --yes")
		}
		fmt.Fprint(opts.out, "\nRelease it? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			return errors.New("release cancelled")
		}
	}

	if _, err := opts.client.ReleaseExternal(deploymentID); err != nil {
		return fmt.Errorf("could not release deployment #%d: %w", deploymentID, err)
	}

	// Clear every attach/pending stamp before anything else. A host left
	// carrying a deployment id whose row is gone is worse than one carrying
	// none: every later verb would address a deployment that no longer exists.
	h.DeploymentID = 0
	h.AttachedAt = ""
	h.PublicHost = ""
	h.PendingDeploymentID = 0
	if opts.keep {
		if err := config.PutHost(h); err != nil {
			return err
		}
	} else if _, err := config.RemoveHost(h.Alias); err != nil {
		return err
	}
	hosts, err := config.LoadHosts()
	if err != nil {
		return err
	}
	if err := syncHostConfig(hosts); err != nil {
		return err
	}

	fmt.Fprintf(opts.out, "\n✓ Released %s. The box is still running. Nothing was torn down.\n", h.Alias)
	return nil
}
