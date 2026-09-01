package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// hostTargetPrefix marks a target as a box in the local registry rather than a
// deployment the control plane knows about. `aq run host:lease-a -- nvidia-smi`.
//
// A prefix, not a bare alias, because the two namespaces must never be guessed
// between: `aq ssh train` is a deployment named train, `aq ssh host:train` is a
// box in the registry, and there is no rule that silently resolves one to the
// other. A wrong guess here would either dial the wrong machine or make an API
// call on a run that is supposed to make none.
const hostTargetPrefix = "host:"

// parseHostTarget splits a `host:<alias>` target. ok is false for every other
// shape, including a bare alias that happens to match a registered host.
func parseHostTarget(target string) (alias string, ok bool) {
	if !strings.HasPrefix(target, hostTargetPrefix) {
		return "", false
	}
	alias = strings.TrimSpace(strings.TrimPrefix(target, hostTargetPrefix))
	if alias == "" {
		return "", false
	}
	return alias, true
}

// isHostTarget reports whether a target addresses the local registry.
func isHostTarget(target string) bool {
	_, ok := parseHostTarget(target)
	return ok
}

// clientForTarget returns an authenticated API client, or nil when the target
// names a detached host.
//
// Returning a real nil is the rail, not an optimization. A detached run must
// never reach the network, and the cheapest way to be sure of that is for there
// to be no client to reach it with: any code path that tries becomes a
// nil-pointer panic in the test suite rather than a quiet fallback to
// DefaultAPIURL that nobody notices until it shows up in an access log.
// TestDetachedVerbsNeverTouchTheAPI pins exactly this.
func clientForTarget(target string) (*api.Client, error) {
	if isHostTarget(target) {
		return nil, nil
	}
	cred, err := requireLogin()
	if err != nil {
		return nil, err
	}
	return newControlClient(cred), nil
}

// newControlClientOrNil is clientForTarget's counterpart for the verbs that
// carry a credential around in their options struct: a nil credential is a
// detached run and yields a nil client rather than dereferencing it.
func newControlClientOrNil(cred *config.Credential) *api.Client {
	if cred == nil {
		return nil
	}
	return newControlClient(cred)
}

// lookupHost resolves an alias against the registry.
func lookupHost(alias string) (config.Host, error) {
	h, ok, err := config.FindHost(alias)
	if err != nil {
		return config.Host{}, err
	}
	if !ok {
		return config.Host{}, fmt.Errorf("no host %q in your registry: add it with `aq host add %s --ssh root@<ip>`, or see `aq host ls`", alias, alias)
	}
	return h, nil
}

// resolveHostSSHAlias refreshes the detached fragment and returns the ssh alias
// for one registered host. No network call, by construction.
func resolveHostSSHAlias(alias string) (string, error) {
	h, err := lookupHost(alias)
	if err != nil {
		return "", err
	}
	hosts, err := config.LoadHosts()
	if err != nil {
		return "", err
	}
	// The alias must exist in the fragment before we exec against it, so unlike
	// the best-effort sync on the managed path this failure is fatal.
	if err := syncHostConfig(hosts); err != nil {
		return "", err
	}
	return hostAliasFor(h.Alias), nil
}

// detachedOgreVerb maps an aq verb onto the ogre verb that answers it on the box.
//
// Detached mode has no orchestrator, so these are not "the same call with a
// different transport" — they are the on-box equivalents, and the mapping is
// stated here rather than spread across the verbs so the whole translation is
// one table a reader can check.
//
//	status    → ogre status    (GPU/snapshot dashboard, straight from the box)
//	save      → ogre snapshot  (capture into the configured BYO bucket)
//	sync-now  → ogre push      (flush this box's snapshots to its configured
//	                            remote; with no scheduler on a detached box,
//	                            "force a tick now" is precisely this)
//	up        → ogre up        (bring services up in place — it never rents
//	                            anything, because the box already exists)
var detachedOgreVerb = map[string]string{
	"status":   "status",
	"save":     "snapshot",
	"sync-now": "push",
	"up":       "up",
}

// detachedOptions configures runDetached.
type detachedOptions struct {
	// client is ALWAYS nil on a detached run. It is present so the rail is
	// visible in the signature and testable with a nil value; nothing here may
	// dereference it.
	client *api.Client
	verb   string
	alias  string
	// args are extra arguments appended to the remote ogre command, already
	// validated by the calling verb.
	args      []string
	printOnly bool
	out       io.Writer
	errOut    io.Writer
	// handoff replaces the aq process with ssh. Tests inject a recorder.
	handoff func(args []string) error
}

// runDetached executes one aq verb against a registered box by driving ogre over
// ssh. The box's own ogre CLI talks to its daemon on loopback, so nothing here
// needs the box to accept an inbound connection from anywhere but the user's
// own ssh session.
func runDetached(opts detachedOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}
	if opts.handoff == nil {
		opts.handoff = execSSH
	}

	ogreVerb, ok := detachedOgreVerb[opts.verb]
	if !ok {
		return fmt.Errorf("`aq %s` does not work on a detached host: it needs the control plane; see `aq help`", opts.verb)
	}

	sshAlias, err := resolveHostSSHAlias(opts.alias)
	if err != nil {
		return err
	}

	remote := shellJoin(append([]string{"ogre", ogreVerb}, opts.args...))
	args := buildSSHArgs(sshAlias, "", nil, []string{remote})

	if opts.printOnly {
		fmt.Fprintln(opts.out, "ssh "+shellJoin(args))
		return nil
	}
	fmt.Fprintf(opts.errOut, "→ %s: %s\n", sshAlias, remote)
	return opts.handoff(args)
}

// directSSHArgs builds an ssh argv for a host that has no generated stanza yet —
// `aq host add`'s preflight, which must be able to survey a box before anything
// is written anywhere, including on the laptop.
//
// Every option that matters is passed explicitly rather than relied on from the
// environment. IdentityFile is absolute (OpenSSH expands `~` from the passwd
// entry, aq resolves it from $HOME, and the two disagree in a container), and
// the known-hosts file is aq's own — a GPU provider recycling an IP into the
// user's real known_hosts is how you get the full-screen host-key warning fired
// at infrastructure they actually care about.
func directSSHArgs(h config.Host, remote string) ([]string, error) {
	dir, err := sshDir()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(dir, knownHostsName),
	}
	if h.Port > 0 {
		args = append(args, "-p", strconv.Itoa(h.Port))
	}
	if h.Identity != "" {
		args = append(args, "-i", h.Identity, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, h.SSH)
	if remote != "" {
		args = append(args, remote)
	}
	return args, nil
}

// remoteRunner runs a remote shell snippet and returns its stdout. Injected so
// every preflight, install and probe path is testable without a box.
type remoteRunner func(h config.Host, remote string) ([]byte, error)

// runRemoteCapture is the real remoteRunner: ssh, capture stdout, surface
// stderr in the error so a failure says what the box said rather than only that
// it exited non-zero.
func runRemoteCapture(h config.Host, remote string) ([]byte, error) {
	args, err := directSSHArgs(h, remote)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return out, fmt.Errorf("ssh %s: %w", h.SSH, err)
		}
		return out, fmt.Errorf("ssh %s: %w (%s)", h.SSH, err, msg)
	}
	return out, nil
}

// hostMountPathFor returns the registered workspace root for a `host:<alias>`
// target, and "" for anything else.
//
// Without it `aq push host:x` would land in /workspace on a box the user
// registered with `--mount-path /data`, and the `aq run` after it would execute
// in a directory the push never wrote to. The mount path is recorded per box
// precisely because it is not /workspace everywhere — a leased machine's data
// volume is wherever its owner mounted it.
//
// A missing registry entry yields "" rather than an error: the caller is about
// to resolve the alias anyway, and that is where an unknown host gets its real
// message.
func hostMountPathFor(target string) string {
	alias, ok := parseHostTarget(target)
	if !ok {
		return ""
	}
	h, found, err := config.FindHost(alias)
	if err != nil || !found {
		return ""
	}
	return h.MountPath
}
