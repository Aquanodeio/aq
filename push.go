package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Aquanodeio/aq/internal/config"
)

// pushOptions configures runPush. push() fills in the real environment; tests
// inject the alias resolver, the rsync probe, and the transfer executor so the
// whole decision path runs without a box.
type pushOptions struct {
	cred       *config.Credential
	target     string // "" → the single live deployment
	from       string // local directory, "" → cwd
	to         string // remote directory, "" → /workspace
	excludes   []string
	noDefaults bool
	del        bool
	printOnly  bool
	out        io.Writer
	errOut     io.Writer

	resolveAlias func(target string, errOut io.Writer) (string, error)
	probeRsync   func(alias string) bool
	transfer     func(p transferPlan, errOut io.Writer) error
}

// push parses `aq push [name|id]` and wires the real environment into runPush.
//
// Sends the working directory to a box you already rented, so the edit → run
// loop is local editor, remote GPU. It is deliberately a plain directory copy
// over the same managed ssh alias `aq ssh` uses — not a new protocol — so
// anything you can do with scp or rsync by hand keeps working alongside it.
func push(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	from := fs.String("from", "", "Local directory to send (default: the current directory)")
	to := fs.String("to", "", "Destination directory on the box (default: "+defaultRemoteDir+")")
	del := fs.Bool("delete", false, "Delete remote files that no longer exist locally (needs rsync)")
	noDefaults := fs.Bool("no-default-excludes", false, "Do not skip .git, node_modules, __pycache__, and friends")
	printOnly := fs.Bool("print", false, "Print the transfer command that would run, and exit")
	var excludes stringList
	fs.Var(&excludes, "exclude", "Skip paths matching this pattern (repeatable)")

	positional, err := parseInterspersed(fs, args)
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

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runPush(pushOptions{
		cred:       cred,
		target:     target,
		from:       *from,
		to:         *to,
		excludes:   excludes,
		noDefaults: *noDefaults,
		del:        *del,
		printOnly:  *printOnly,
		out:        os.Stdout,
		errOut:     os.Stderr,
	})
}

// runPush resolves the box and sends the tree.
func runPush(opts pushOptions) error {
	opts = opts.withDefaults()

	alias, err := opts.resolveAlias(opts.target, opts.errOut)
	if err != nil {
		return err
	}
	return pushToAlias(alias, opts)
}

// pushToAlias is the half of a push that runs once the box is already
// resolved. `aq run` calls it directly so a run resolves the deployment once
// rather than once per phase.
func pushToAlias(alias string, opts pushOptions) error {
	opts = opts.withDefaults()

	from, err := resolveLocalDir(opts.from)
	if err != nil {
		return err
	}
	to, err := validateRemoteDir(opts.to)
	if err != nil {
		return err
	}
	excludes, err := resolveExcludes(from, opts.excludes, !opts.noDefaults)
	if err != nil {
		return err
	}

	plan, err := buildPlan(alias, from, to, excludes, opts.del, opts.probeRsync(alias))
	if err != nil {
		return err
	}

	if opts.printOnly {
		fmt.Fprintln(opts.out, plan.describe())
		return nil
	}

	// Echo to stderr so the user can see which transport they got — a tar-mode
	// push re-sends the whole tree, and knowing that is the difference between
	// "aq is slow" and "this box has no rsync".
	fmt.Fprintf(opts.errOut, "→ %s → %s:%s (%s)\n", from, alias, to, plan.mode)
	if err := opts.transfer(plan, opts.errOut); err != nil {
		return err
	}
	fmt.Fprintf(opts.errOut, "✓ pushed to %s:%s\n", alias, to)
	return nil
}

// withDefaults fills in the writers and the injected collaborators.
func (o pushOptions) withDefaults() pushOptions {
	if o.out == nil {
		o.out = os.Stdout
	}
	if o.errOut == nil {
		o.errOut = os.Stderr
	}
	if o.resolveAlias == nil {
		cred := o.cred
		o.resolveAlias = func(target string, errOut io.Writer) (string, error) {
			return resolveSSHAlias(newControlClient(cred), target, "push to", errOut)
		}
	}
	if o.probeRsync == nil {
		o.probeRsync = probeRemoteRsync
	}
	if o.transfer == nil {
		o.transfer = runTransfer
	}
	return o
}
