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
		return fmt.Errorf("expected at most one deployment — got %s", strings.Join(positional, ", "))
	}

	var target string
	if len(positional) == 1 {
		target = positional[0]
	}
	// A positional that looks like a flag would otherwise be smuggled through to
	// ssh as one, mirroring the validate-before-exec habit openBrowser follows.
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid deployment %q — it must not start with '-'", target)
	}

	cred, err := requireLogin()
	if err != nil {
		return err
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

	client := newControlClient(opts.cred)

	deploymentID, err := resolveDeploymentID(client, opts.target, "ssh")
	if err != nil {
		return err
	}

	res, err := client.DeploymentStatus(deploymentID)
	if err != nil {
		return fmt.Errorf("could not fetch status for deployment #%d: %w", deploymentID, err)
	}
	dep := res.Deployment
	if dep.ID == 0 {
		dep.ID = deploymentID
	}
	state := dep.Status
	if state == "" {
		state = res.Status
	}

	switch {
	case isClosedStatus(state):
		return fmt.Errorf("deployment #%d is %s — it is no longer running", deploymentID, state)
	case !isActiveStatus(state):
		return fmt.Errorf("deployment #%d is still provisioning (%s) — retry in a minute", deploymentID, state)
	}

	if _, _, ok := sshEndpointFor(dep); !ok {
		// Distinct from the provisioning message on purpose: a box whose provider
		// maps no port 22 (an Akash lease without an SSH mapping) will never grow
		// one, so "retry in a minute" would be a lie.
		if dep.AppURL == "" && len(dep.ServiceURLs) == 0 {
			return fmt.Errorf("deployment #%d is up but has not reported an address yet — retry in a moment", deploymentID)
		}
		return fmt.Errorf("deployment #%d does not expose SSH", deploymentID)
	}

	warnIfKeyUnregistered(client, opts.errOut)

	// The alias must exist before we exec, and it is what we exec against — so
	// unlike the up/status paths this sync is fatal.
	if err := syncManagedConfig(client, []api.Deployment{dep}, 0); err != nil {
		return err
	}

	alias := aliasFor(dep.Name, dep.ID)
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
		fmt.Fprintf(errOut, "warning: no local SSH key found — run `aq up` to have aq create one.\n")
		return
	}
	keys, err := client.ListSSHKeys()
	if err != nil {
		return
	}
	if _, matched := matchRegisteredKey(key.PublicKey, keys); !matched {
		fmt.Fprintf(errOut, "warning: %s is not registered on your account — this box may not accept it.\n", key.PublicPath)
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
