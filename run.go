package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// runOptions configures runRun. runCmd fills in the real environment; tests
// inject the resolver, the push step, and the ssh handoff.
type runOptions struct {
	cred    *config.Credential
	target  string
	command []string // everything after a literal `--`
	push    pushOptions
	noPush  bool
	detach  bool
	dir     string // remote working directory, "" → push destination
	print   bool
	// thenPauseMinutes is --then-pause's act-after window in whole minutes;
	// 0 means the flag was not given. Only meaningful with detach: runCmd
	// refuses it locally otherwise, since a foreground run leaves nothing
	// running to arm a pause on once it returns.
	thenPauseMinutes int
	out              io.Writer
	errOut           io.Writer

	resolveAlias func(target string, errOut io.Writer) (string, error)
	doPush       func(alias string, opts pushOptions) error
	handoff      func(args []string) error
	launch       func(alias, workdir string, command []string) (string, error)
	// armThenPause enables idle auto-pause on the deployment behind target,
	// for a --then-pause detached run, and returns the resolved deployment
	// id so the caller can print exactly what got armed. Tests inject a
	// stub; runRun defaults it to a real idle-policy call.
	armThenPause func(target string, actAfterMinutes int) (deploymentID int, err error)
}

// runCmd parses `aq run [name|id] -- <command…>`.
//
// One command for the whole loop: send the working directory to the box, then
// run something in it with the terminal attached. It is `aq push` followed by
// `aq ssh -- cd <dir> && <cmd>`, which is exactly what people were typing by
// hand.
func runCmd(args []string) error {
	head, command := splitRemoteCommand(args)

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	from := fs.String("from", "", "Local directory to send (default: the current directory)")
	to := fs.String("to", "", "Destination directory on the box (default: "+defaultRemoteDir+")")
	dir := fs.String("dir", "", "Directory to run in on the box (default: the push destination)")
	del := fs.Bool("delete", false, "Delete remote files that no longer exist locally (needs rsync)")
	noDefaults := fs.Bool("no-default-excludes", false, "Do not skip .git, node_modules, __pycache__, and friends")
	includeSecrets := fs.Bool("include-secrets", false, "Also send .env, SSH keys, and other credential-shaped paths (skipped by default)")
	noPush := fs.Bool("no-push", false, "Run without sending the working directory first")
	detach := fs.Bool("detach", false, "Start the command and return: it keeps running after you disconnect")
	thenPause := fs.String("then-pause", "", "Valid only with --detach. After launch, arm idle auto-pause on the deployment for this act-after window (e.g. 1h): it pauses once the command has finished AND the GPU has stayed idle that long, not the instant the process exits")
	printOnly := fs.Bool("print", false, "Print the commands that would run, and exit")
	var excludes stringList
	fs.Var(&excludes, "exclude", "Skip paths matching this pattern (repeatable)")

	positional, err := parseInterspersed(fs, head)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fmt.Errorf("expected at most one deployment, got %s", strings.Join(positional, ", "))
	}
	var target string
	if len(positional) == 1 {
		target = positional[0]
	}
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid deployment %q; it must not start with '-'", target)
	}
	if len(command) == 0 {
		return fmt.Errorf("a command is required, usage: aq run [name|id] -- <command…>")
	}

	var thenPauseMinutes int
	if *thenPause != "" {
		if !*detach {
			return fmt.Errorf("--then-pause requires --detach: a foreground run leaves nothing running to arm a pause on once it returns")
		}
		if isHostTarget(target) {
			return fmt.Errorf("--then-pause needs the platform's idle-policy API, which a detached host: target never calls")
		}
		m, err := parsePositiveMinutes("--then-pause", *thenPause)
		if err != nil {
			return err
		}
		thenPauseMinutes = m
	}

	var cred *config.Credential
	if !isHostTarget(target) {
		if cred, err = requireLogin(); err != nil {
			return err
		}
	}

	// Same as `aq push`: an unflagged destination on a detached box is that
	// box's registered workspace root. --dir still defaults to wherever the
	// push landed, so the two cannot disagree.
	dest := *to
	if dest == "" {
		dest = hostMountPathFor(target)
	}

	return runRun(runOptions{
		cred:             cred,
		target:           target,
		command:          command,
		noPush:           *noPush,
		detach:           *detach,
		thenPauseMinutes: thenPauseMinutes,
		dir:              *dir,
		print:            *printOnly,
		push: pushOptions{
			cred:           cred,
			target:         target,
			from:           *from,
			to:             dest,
			excludes:       excludes,
			noDefaults:     *noDefaults,
			includeSecrets: *includeSecrets,
			del:            *del,
			printOnly:      *printOnly,
		},
		out:    os.Stdout,
		errOut: os.Stderr,
	})
}

// runRun pushes, then hands the terminal to ssh running the command.
func runRun(opts runOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}
	if opts.resolveAlias == nil {
		cred := opts.cred
		opts.resolveAlias = func(target string, errOut io.Writer) (string, error) {
			return resolveSSHAlias(newControlClientOrNil(cred), target, "run on", errOut)
		}
	}
	if opts.doPush == nil {
		opts.doPush = pushToAlias
	}
	if opts.handoff == nil {
		opts.handoff = execSSH
	}
	if opts.launch == nil {
		opts.launch = func(alias, workdir string, command []string) (string, error) {
			return launchDetached(alias, workdir, command, nil)
		}
	}
	if opts.armThenPause == nil {
		cred := opts.cred
		opts.armThenPause = func(target string, actAfterMinutes int) (int, error) {
			client := newControlClient(cred)
			deploymentID, err := resolveDeploymentID(client, target, "run --then-pause")
			if err != nil {
				return 0, err
			}
			enabled := true
			m := actAfterMinutes
			if _, err := client.SetIdlePolicy(deploymentID, api.IdlePolicyUpdate{
				ActAfterMinutes:  &m,
				AutoPauseEnabled: &enabled,
			}); err != nil {
				return 0, err
			}
			return deploymentID, nil
		}
	}

	// --dir defaults to wherever the push landed, so `aq run -- python train.py`
	// runs against the files it just sent rather than in the login home.
	remoteDir, err := validateRemoteDir(opts.dir)
	if err != nil {
		return err
	}
	if opts.dir == "" {
		if remoteDir, err = validateRemoteDir(opts.push.to); err != nil {
			return err
		}
	}

	alias, err := opts.resolveAlias(opts.target, opts.errOut)
	if err != nil {
		return err
	}

	if !opts.noPush {
		p := opts.push
		p.out, p.errOut = opts.out, opts.errOut
		if err := opts.doPush(alias, p); err != nil {
			return err
		}
	}

	if opts.detach {
		if opts.print {
			fmt.Fprintln(opts.out, "ssh "+shellJoin(buildSSHArgs(alias, "", nil,
				[]string{buildDetachedRunScript(remoteDir, opts.command)})))
			return nil
		}
		id, err := opts.launch(alias, remoteDir, opts.command)
		if err != nil {
			return err
		}
		// The run id goes to stdout alone so `RUN=$(aq run --detach -- …)` works;
		// everything a human needs is on stderr beside it.
		fmt.Fprintln(opts.out, id)
		fmt.Fprintf(opts.errOut, "→ %s: %s (detached, run %s)\n", alias, strings.Join(opts.command, " "), id)
		fmt.Fprintf(opts.errOut, "  follow it with `aq logs %s-f`\n", displayTarget(opts.target))

		if opts.thenPauseMinutes > 0 {
			deploymentID, err := opts.armThenPause(opts.target, opts.thenPauseMinutes)
			if err != nil {
				return fmt.Errorf("run %s started, but could not arm --then-pause: %w", id, err)
			}
			fmt.Fprintf(opts.errOut, "✓ Armed idle auto-pause on deployment #%d: pauses after %s of sustained GPU idle once this command finishes (not the instant it exits)\n",
				deploymentID, formatMinutes(opts.thenPauseMinutes))
		}
		return nil
	}

	remote := []string{remoteCommand(remoteDir, opts.command)}
	args := buildSSHArgs(alias, "", nil, remote)

	if opts.print {
		fmt.Fprintln(opts.out, "ssh "+shellJoin(args))
		return nil
	}
	fmt.Fprintf(opts.errOut, "→ %s: %s\n", alias, strings.Join(opts.command, " "))
	return opts.handoff(args)
}

// remoteCommand wraps the user's command in a cd to the working directory.
//
// The command is joined and passed to the remote login shell as one string —
// the same thing ssh does with a trailing command — so shell syntax the user
// typed (pipes, redirects, &&) keeps working instead of being quoted into a
// literal argument.
func remoteCommand(dir string, command []string) string {
	return "cd " + shellQuote(dir) + " && " + strings.Join(command, " ")
}

// displayTarget renders the deployment the user named, for echoing a follow-up
// command back at them. An empty target means they relied on having exactly one
// live box, so the suggestion should too.
func displayTarget(target string) string {
	if strings.TrimSpace(target) == "" {
		return ""
	}
	return target + " "
}
