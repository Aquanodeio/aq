package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
	out     io.Writer
	errOut  io.Writer

	resolveAlias func(target string, errOut io.Writer) (string, error)
	doPush       func(alias string, opts pushOptions) error
	handoff      func(args []string) error
	launch       func(alias, workdir string, command []string) (string, error)
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
	noPush := fs.Bool("no-push", false, "Run without sending the working directory first")
	detach := fs.Bool("detach", false, "Start the command and return — it keeps running after you disconnect")
	printOnly := fs.Bool("print", false, "Print the commands that would run, and exit")
	var excludes stringList
	fs.Var(&excludes, "exclude", "Skip paths matching this pattern (repeatable)")

	positional, err := parseInterspersed(fs, head)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fmt.Errorf("expected at most one deployment — got %s", strings.Join(positional, ", "))
	}
	var target string
	if len(positional) == 1 {
		target = positional[0]
	}
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid deployment %q — it must not start with '-'", target)
	}
	if len(command) == 0 {
		return fmt.Errorf("a command is required — usage: aq run [name|id] -- <command…>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runRun(runOptions{
		cred:    cred,
		target:  target,
		command: command,
		noPush:  *noPush,
		detach:  *detach,
		dir:     *dir,
		print:   *printOnly,
		push: pushOptions{
			cred:       cred,
			target:     target,
			from:       *from,
			to:         *to,
			excludes:   excludes,
			noDefaults: *noDefaults,
			del:        *del,
			printOnly:  *printOnly,
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
			return resolveSSHAlias(newControlClient(cred), target, "run on", errOut)
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
