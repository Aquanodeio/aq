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

// sshOptions configures runSSH. sshCmd fills in the real environment; tests
// inject a base URL, buffer writers, and a stub handoff.
type sshOptions struct {
	cred      *config.Credential
	target    string // "" → the single live deployment
	user      string // "" → root
	forwards  []string
	remote    []string // remote command, everything after a literal `--`
	printOnly bool
	out       io.Writer
	errOut    io.Writer
	// handoff replaces the aq process with ssh. Tests inject a recorder;
	// runSSH defaults it to execSSH.
	handoff func(args []string) error
}

// stringList collects a repeatable string flag (`-L a -L b`).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, " ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// sshCmd parses flags and wires the real environment into runSSH.
//
// `aq ssh [name|id]` opens a shell on a box rented by `aq up` / `aq deploy`,
// with no key to create, no IP to copy, and no id to remember.
func sshCmd(args []string) error {
	head, remote := splitRemoteCommand(args)

	fs := flag.NewFlagSet("ssh", flag.ContinueOnError)
	printOnly := fs.Bool("print", false, "Print the ssh command that would run, and exit")
	user := fs.String("user", "", "Override the login user (default: root)")
	var forwards stringList
	fs.Var(&forwards, "L", "Forward a local port, e.g. 8888:localhost:8888 (repeatable)")
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
	// A positional that looks like a flag would otherwise be smuggled through to
	// ssh as one, mirroring the validate-before-exec habit openBrowser follows.
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid deployment %q; it must not start with '-'", target)
	}

	// A detached host needs no login at all — not even a credential file. That
	// is the whole point of detached mode, so the login check is skipped rather
	// than made lenient.
	var cred *config.Credential
	if !isHostTarget(target) {
		if cred, err = requireLogin(); err != nil {
			return err
		}
	}

	return runSSH(sshOptions{
		cred:      cred,
		target:    target,
		user:      *user,
		forwards:  forwards,
		remote:    remote,
		printOnly: *printOnly,
		out:       os.Stdout,
		errOut:    os.Stderr,
	})
}

// splitRemoteCommand splits args at the first bare `--`. Everything after it is
// the command to run on the box; everything before it is aq's own flags.
func splitRemoteCommand(args []string) (head, remote []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// runSSH resolves the target, refreshes the ssh_config fragment, and hands the
// terminal to the real ssh binary.
func runSSH(opts sshOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}
	if opts.handoff == nil {
		opts.handoff = execSSH
	}

	client := newControlClientOrNil(opts.cred)

	alias, err := resolveSSHAlias(client, opts.target, "ssh", opts.errOut)
	if err != nil {
		return err
	}

	args := buildSSHArgs(alias, opts.user, opts.forwards, opts.remote)

	if opts.printOnly {
		fmt.Fprintln(opts.out, "ssh "+strings.Join(args, " "))
		return nil
	}
	// Echo to stderr, so the user learns the alias by seeing it and `--print`
	// keeps stdout clean for scripting.
	fmt.Fprintf(opts.errOut, "→ ssh %s\n", strings.Join(args, " "))
	return opts.handoff(args)
}

// buildSSHArgs assembles ssh's argv (excluding argv[0]).
//
// It targets the generated alias rather than root@<ip> deliberately: what aq
// runs is then exactly what the user can retype, and the same alias is what
// scp, rsync, and VSCode Remote-SSH take — three capabilities for the price of
// one file write, and the reason aq needs no `aq scp` of its own.
func buildSSHArgs(alias, user string, forwards, remote []string) []string {
	var args []string
	for _, f := range forwards {
		args = append(args, "-L", f)
	}
	if user != "" {
		args = append(args, "-l", user)
	}
	args = append(args, alias)
	return append(args, remote...)
}

// warnIfKeyUnregistered tells the user up front when the local key aq would use
// is not one the account has registered.
//
// It warns rather than fails, and it deliberately does NOT register a key: the
// box's authorized_keys was fixed at provision time from the sshKeyId passed to
// POST /deployments/up, so registering one now would change nothing on an
// already-running box while printing a reassuring message and then failing to
// connect. A warning is also right rather than an error because the user may be
// authenticating from an agent aq cannot see.
func warnIfKeyUnregistered(client *api.Client, errOut io.Writer) {
	key, ok, err := findLocalKey()
	if err != nil || !ok {
		fmt.Fprintf(errOut, "warning: no local SSH key found; run `aq up` to have aq create one.\n")
		return
	}
	keys, err := client.ListSSHKeys()
	if err != nil {
		return
	}
	if _, matched := matchRegisteredKey(key.PublicKey, keys); !matched {
		fmt.Fprintf(errOut, "warning: %s is not registered on your account; this box may not accept it.\n", key.PublicPath)
	}
}

// sshEndpointFor resolves a deployment's SSH host and port.
//
// service_urls wins over app_url: it is keyed by the box's *internal* port, so
// its port-22 entry is unambiguously sshd and is simply absent on a box with no
// SSH. app_url only approximates that — simplepod publishes the ogre agent's
// HTTP port there when the box maps no SSH port, which aq would otherwise dial
// and hang on with nothing in the error hinting why.
func sshEndpointFor(dep api.Deployment) (host, port string, ok bool) {
	if u, found := dep.SSHServiceURL(); found {
		if host, port, ok = sshEndpoint(u); ok {
			return host, port, true
		}
	}
	return sshEndpoint(dep.AppURL)
}
