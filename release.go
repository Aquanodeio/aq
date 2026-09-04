package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// releaseOptions configures runRelease.
type releaseOptions struct {
	alias string
	yes   bool
	keep  bool
	// force hands the box back even when aq could not reach it to clean it up.
	// A user is entitled to end our relationship with their machine whether or
	// not the machine answers. Refusing outright would strand the row and
	// change nothing on the box. What it must never do is look like a clean
	// hand-back, so the flag exists to make the choice explicit and the output
	// says exactly what is still on the machine.
	force  bool
	out    io.Writer
	errOut io.Writer

	client *api.Client
	run    remoteRunner
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
	force := fs.Bool("force", false, "Release even if aq cannot reach the box to stop its agent and remove our credentials")
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
		force:  *force,
		out:    os.Stdout,
		errOut: os.Stderr,
		client: newControlClient(cred),
		run:    runRemoteCapture,
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
	if opts.run == nil {
		opts.run = runRemoteCapture
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
		fmt.Fprintln(opts.out, "  • Aquanode revokes this box's credentials and removes the ones it pushed to the box")
		fmt.Fprintln(opts.out, "  • aq stops the ogre daemon it started, over ssh, and removes "+ogreEnvPath)
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

	// The orchestrator clears everything reachable over ogre's own API and tells
	// us three-state whether it managed to. It cannot stop the daemon: ogre
	// deliberately has no shutdown endpoint, so that half is ours, below.
	released, err := opts.client.ReleaseExternal(deploymentID)
	if err != nil {
		return fmt.Errorf("could not release deployment #%d: %w", deploymentID, err)
	}

	// Stopping the agent is the half that actually matters for what happens
	// NEXT. A released box whose ogre keeps running still owns the control port,
	// so the obvious next thing a user does, attach the same box again, races
	// that daemon. Do it here, over the same ssh path attach used to start it.
	//
	// It runs AFTER the row is gone rather than before, because the row is what
	// the user asked us to drop and a machine we cannot reach must not be able
	// to hold their account hostage. That ordering is only safe because the
	// failure is LOUD: an unreachable box prints exactly what is still on it and
	// exactly how to finish the job by hand.
	// Captured BEFORE the stamps below are cleared: `h.Attached()` is false the
	// moment they are, and reading it afterwards would silently skip the whole
	// report on every release.
	wasAttached := h.Attached()
	ogrePort := h.OgrePort
	if ogrePort == 0 {
		ogrePort = defaultOgrePort
	}
	var stopErr error
	if wasAttached {
		stopErr = stopOgreOnBox(opts, h)
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
	return reportReleaseCleanup(opts, releaseCleanupContext{
		alias:       h.Alias,
		ogrePort:    ogrePort,
		wasAttached: wasAttached,
	}, released, stopErr)
}

// releaseOgreScript stops the ogre daemon aq started and removes the credential
// file it wrote.
//
// Same pattern discipline as restartOgreScript: the match string is assembled
// on the box from a variable so it never appears literally in the script text,
// which is itself in the remote shell's /proc/<pid>/cmdline. Spelled out, the
// script kills the ssh session running it.
//
// It WAITS for the daemon to go, for the same reason the restart does: a
// release whose daemon is still winding down when the user re-attaches puts the
// new process straight into ogre's second-instance guard. Waiting here is the
// cheap half of never running that race.
//
// The env file is removed rather than edited: it holds only what aq wrote, its
// whole purpose was to authenticate a deployment that no longer exists, and
// leaving a JWT secret behind for a row nobody can reach is pure downside.
func releaseOgreScript(port int) string {
	p := strconv.Itoa(port)
	return "AQ_OGRE_PORT=" + p + "\n" +
		`AQ_OGRE_PAT="ogre -port ${AQ_OGRE_PORT}"` + "\n" +
		"aq_pids() {\n" +
		"  for pid in $(pgrep -f -- \"$AQ_OGRE_PAT\" 2>/dev/null); do\n" +
		"    [ \"$pid\" = \"$$\" ] && continue\n" +
		"    echo \"$pid\"\n" +
		"  done\n" +
		"}\n" +
		"for pid in $(aq_pids); do kill \"$pid\" >/dev/null 2>&1 || true; done\n" +
		"AQ_WAIT=0\n" +
		"while [ \"$AQ_WAIT\" -lt 30 ] && [ -n \"$(aq_pids)\" ]; do\n" +
		"  sleep 1\n" +
		"  AQ_WAIT=$((AQ_WAIT + 1))\n" +
		"done\n" +
		"if [ -n \"$(aq_pids)\" ]; then\n" +
		"  for pid in $(aq_pids); do kill -9 \"$pid\" >/dev/null 2>&1 || true; done\n" +
		"  AQ_WAIT=0\n" +
		"  while [ \"$AQ_WAIT\" -lt 5 ] && [ -n \"$(aq_pids)\" ]; do\n" +
		"    sleep 1\n" +
		"    AQ_WAIT=$((AQ_WAIT + 1))\n" +
		"  done\n" +
		"fi\n" +
		"rm -f " + shellQuote(ogreEnvPath) + " >/dev/null 2>&1 || true\n" +
		// Assert the OUTCOME, not the commands. A kill that returned 0 is not a
		// process that exited, and this is the only line that can tell the
		// difference.
		"if [ -n \"$(aq_pids)\" ]; then\n" +
		"  echo \"ogre on port ${AQ_OGRE_PORT} is still running\" >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"echo release_ok=1\n"
}

// stopOgreOnBox runs releaseOgreScript and turns anything short of a confirmed
// stop into an error. "The ssh call returned 0" is not the same fact as "the
// daemon is gone", so the script prints a marker on the one path where it is,
// and its absence is a failure here even when ssh itself was happy.
func stopOgreOnBox(opts releaseOptions, h config.Host) error {
	port := h.OgrePort
	if port == 0 {
		port = defaultOgrePort
	}
	out, err := opts.run(h, releaseOgreScript(port))
	if err != nil {
		return fmt.Errorf("could not reach %s over ssh: %w", h.SSH, err)
	}
	if !strings.Contains(string(out), "release_ok=1") {
		return fmt.Errorf("the box did not confirm ogre stopped on port %d", port)
	}
	return nil
}

// reportReleaseCleanup says what actually happened on the machine.
//
// THREE-STATE ON BOTH HALVES, AND NEITHER MAY BE FOLDED INTO THE SUCCESS LINE.
// The row is gone either way (that part is done and is not in doubt) but
// "we cleaned the box" and "we could not look" are different facts about
// somebody's hardware, and only one of them is a clean hand-back. An
// orchestrator that predates `boxCleared` reports nil, which is UNKNOWN and
// prints as such rather than being optimistically read as done.
//
// It returns an error when anything is unresolved, because the exit code is
// what a script would read, and a script must not conclude from a 0 that a
// customer's box carries none of our credentials.
type releaseCleanupContext struct {
	alias       string
	ogrePort    int
	wasAttached bool
}

func reportReleaseCleanup(opts releaseOptions, ctx releaseCleanupContext, released *api.ReleaseResult, stopErr error) error {
	if !ctx.wasAttached {
		// Nothing was ever configured on this box, so there is nothing to clean
		// and nothing to be unsure about.
		return nil
	}

	var unresolved []string

	switch {
	case released == nil || released.BoxCleared == nil:
		unresolved = append(unresolved, "Aquanode could not confirm it removed the credentials it pushed to the box")
	case !*released.BoxCleared:
		unresolved = append(unresolved, "the box refused to remove the credentials Aquanode pushed to it")
	default:
		fmt.Fprintln(opts.out, "  Aquanode's credentials were removed from the box.")
	}
	if released != nil && released.BoxClearReason != "" {
		fmt.Fprintf(opts.out, "    reason: %s\n", released.BoxClearReason)
	}

	if stopErr != nil {
		unresolved = append(unresolved, "aq could not stop the ogre daemon it started: "+stopErr.Error())
	} else {
		fmt.Fprintf(opts.out, "  ogre was stopped and %s removed.\n", ogreEnvPath)
	}

	if len(unresolved) == 0 {
		return nil
	}

	fmt.Fprintln(opts.errOut, "\nThe row is gone, but the box was NOT fully cleaned:")
	for _, u := range unresolved {
		fmt.Fprintf(opts.errOut, "  • %s\n", u)
	}
	fmt.Fprintf(opts.errOut, "\nAn ogre may still be running on %s port %d with credentials Aquanode has\n"+
		"already revoked. It cannot be used, but it owns that port, so attaching this box\n"+
		"again will fail until it is gone. Finish by hand on the box:\n"+
		"  pkill -f 'ogre -port %d' && rm -f %s\n"+
		"  rm -f ~/.config/ogre/config.yaml   # only if you want ogre's stored credentials gone too\n",
		ctx.alias, ctx.ogrePort, ctx.ogrePort, ogreEnvPath)
	if opts.force {
		fmt.Fprintln(opts.errOut, "\n--force was passed, so this is reported rather than fatal.")
		return nil
	}
	return errors.New("released the row, but could not confirm the box is clean (see above; --force to accept this)")
}
