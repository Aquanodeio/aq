package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Aquanodeio/aq/internal/config"
)

// defaultHostMountPath is the workspace root aq assumes on a box it did not
// provision. Same default as `aq push`'s destination, for the same reason: the
// two have to agree or a `run` executes somewhere a `push` never landed.
const defaultHostMountPath = defaultRemoteDir

// ogreInstallPath is where `aq host add` puts an uploaded ogre binary. A
// system-wide bin dir rather than a user one because the boxes this runs against
// log in as root and every later verb resolves `ogre` off PATH in a
// non-interactive shell, which does not source a login profile.
const ogreInstallPath = "/usr/local/bin/ogre"

// hostOptions configures runHost. hostCmd fills in the real environment; tests
// inject the remote runner and the uploader so the whole decision path runs
// without a box.
type hostOptions struct {
	sub       string // add | ls | rm
	alias     string
	ssh       string
	identity  string
	mountPath string
	ogrePort  int
	ogreBin   string
	dryRun    bool
	out       io.Writer
	errOut    io.Writer

	run    remoteRunner
	upload func(h config.Host, localPath, remotePath string) error
	// syncConfig regenerates the ssh_config fragment. Injected so tests never
	// touch the operator's real ~/.ssh — see the CLAUDE.md gotcha: $HOME does
	// not control where OpenSSH resolves `~`, so an env-var sandbox is a lie.
	syncConfig func(hosts []config.Host) error
}

// hostCmd parses `aq host <add|ls|rm>` and wires the real environment.
//
// Detached mode: a box the user owns or leases, that Aquanode never provisioned
// and never dials. aq drives ogre on it over the user's own ssh session, and
// ogre's CLI reaches its daemon on loopback — so the box needs no inbound
// connectivity from us at all, and no Aquanode login is required for any of it.
func hostCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq host <add|ls|rm> [flags] — see `aq help`")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("host "+sub, flag.ContinueOnError)
	ssh := fs.String("ssh", "", "SSH target for the box, e.g. root@1.2.3.4")
	identity := fs.String("identity", "", "Private key to authenticate with (default: the key aq already uses)")
	mountPath := fs.String("mount-path", "", "Workspace root on the box (default: "+defaultHostMountPath+")")
	ogrePort := fs.Int("ogre-port", 0, "Port ogre's control API should listen on once attached (default: "+strconv.Itoa(defaultOgrePort)+")")
	ogreBin := fs.String("ogre-binary", "", "Upload this local Linux x86_64 ogre binary when the box has none")
	dryRun := fs.Bool("dry-run", false, "Survey the box and print the plan; write nothing, anywhere")
	positional, err := parseInterspersed(fs, rest)
	if err != nil {
		return err
	}

	var alias string
	if len(positional) > 0 {
		alias = positional[0]
	}
	if len(positional) > 1 {
		return fmt.Errorf("expected at most one alias — got %s", strings.Join(positional, ", "))
	}

	return runHost(hostOptions{
		sub:       sub,
		alias:     alias,
		ssh:       strings.TrimSpace(*ssh),
		identity:  strings.TrimSpace(*identity),
		mountPath: strings.TrimSpace(*mountPath),
		ogrePort:  *ogrePort,
		ogreBin:   strings.TrimSpace(*ogreBin),
		dryRun:    *dryRun,
		out:       os.Stdout,
		errOut:    os.Stderr,
	})
}

func (o hostOptions) withDefaults() hostOptions {
	if o.out == nil {
		o.out = os.Stdout
	}
	if o.errOut == nil {
		o.errOut = os.Stderr
	}
	if o.run == nil {
		o.run = runRemoteCapture
	}
	if o.upload == nil {
		o.upload = uploadToHost
	}
	if o.syncConfig == nil {
		o.syncConfig = syncHostConfig
	}
	return o
}

// runHost dispatches the three subcommands.
func runHost(opts hostOptions) error {
	opts = opts.withDefaults()
	switch opts.sub {
	case "add":
		return runHostAdd(opts)
	case "ls", "list":
		return runHostLs(opts)
	case "rm", "remove":
		return runHostRm(opts)
	default:
		return fmt.Errorf("unknown host subcommand %q — expected add, ls, or rm", opts.sub)
	}
}

// validHostAlias keeps an alias to what can be an ssh_config Host token and a
// filename component without quoting. The alias is interpolated into a
// generated ssh_config and into remote shell commands, so it is restricted here
// rather than escaped in five places later.
func validHostAlias(alias string) error {
	if alias == "" {
		return errors.New("an alias is required — usage: aq host add <alias> --ssh root@<ip>")
	}
	if len(alias) > 40 {
		return fmt.Errorf("alias %q is too long — keep it to 40 characters", alias)
	}
	for i, r := range alias {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return fmt.Errorf("invalid alias %q — use lowercase letters, digits, '-' and '_', starting with a letter or digit", alias)
		}
	}
	return nil
}

// hostSurvey is what the box told us about itself. Every field is three-state
// where it can be: "we could not look" is never rendered as a reassuring value.
type hostSurvey struct {
	Uname        string
	Arch         string
	OgrePath     string
	OgreVersion  string
	MountExists  bool
	MountChecked bool
	GPU          string
	Daemon       string // ok | unreachable | unknown
	DaemonReason string
}

// surveyScript is read-only. It runs no installer, writes no file, and starts
// nothing — `aq host add --dry-run` must be able to run it against a box the
// user has not decided to register yet, and come away having changed nothing.
//
// It deliberately does not `set -e`: half these probes are expected to fail on
// a given box (no nvidia-smi, no ogre yet), and an early exit would report a
// perfectly good box as unreadable.
func surveyScript(mountPath string) string {
	return strings.Join([]string{
		`echo "uname=$(uname -s 2>/dev/null)"`,
		`echo "arch=$(uname -m 2>/dev/null)"`,
		`echo "ogre_path=$(command -v ogre 2>/dev/null)"`,
		`echo "ogre_version=$(ogre version 2>/dev/null | head -1)"`,
		`if [ -d ` + shellQuote(mountPath) + ` ]; then echo "mount=yes"; else echo "mount=no"; fi`,
		`echo "gpu=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)"`,
		`echo "ok=1"`,
	}, "\n") + "\n"
}

// parseSurvey decodes the key=value lines surveyScript emits.
func parseSurvey(raw []byte) (hostSurvey, error) {
	s := hostSurvey{Daemon: "unknown"}
	sawOK := false
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		switch key {
		case "uname":
			s.Uname = value
		case "arch":
			s.Arch = value
		case "ogre_path":
			s.OgrePath = value
		case "ogre_version":
			s.OgreVersion = value
		case "mount":
			s.MountChecked = true
			s.MountExists = value == "yes"
		case "gpu":
			s.GPU = value
		case "ok":
			sawOK = true
		}
	}
	if !sawOK {
		// A survey that did not run to completion is not a survey with some
		// fields missing — it is a box we could not look at, and every decision
		// downstream depends on having looked.
		return hostSurvey{}, errors.New("the box did not complete the survey — aq could not read enough to proceed safely")
	}
	return s, nil
}

// runHostAdd surveys the box, installs ogre if it needs to, proves the daemon
// answers on loopback, and only then records the entry.
func runHostAdd(opts hostOptions) error {
	if err := validHostAlias(opts.alias); err != nil {
		return err
	}
	if opts.ssh == "" {
		return fmt.Errorf("--ssh is required — usage: aq host add %s --ssh root@<ip>", opts.alias)
	}
	sshTarget, port, err := parseSSHTarget(opts.ssh)
	if err != nil {
		return err
	}
	identity, err := resolveHostIdentity(opts.identity)
	if err != nil {
		return err
	}
	mountPath := opts.mountPath
	if mountPath == "" {
		mountPath = defaultHostMountPath
	}
	if _, err := validateRemoteDir(mountPath); err != nil {
		return err
	}
	ogrePort := opts.ogrePort
	if ogrePort == 0 {
		ogrePort = defaultOgrePort
	}

	h := config.Host{
		Alias:     opts.alias,
		SSH:       sshTarget,
		Port:      port,
		Identity:  identity,
		MountPath: mountPath,
		OgrePort:  ogrePort,
	}

	if existing, found, err := config.FindHost(opts.alias); err != nil {
		return err
	} else if found && !opts.dryRun {
		return fmt.Errorf("host %q is already registered (%s) — remove it with `aq host rm %s` first", existing.Alias, existing.SSH, existing.Alias)
	}

	// 1. Survey. No write, on the box or on this machine. Same posture as
	// `aq import`: the user sees what aq found before anything changes.
	raw, err := opts.run(h, surveyScript(mountPath))
	if err != nil {
		return fmt.Errorf("could not reach %s over ssh: %w", h.SSH, err)
	}
	survey, err := parseSurvey(raw)
	if err != nil {
		return err
	}
	if survey.OgrePath != "" {
		survey.Daemon, survey.DaemonReason = probeOgreDaemon(opts.run, h)
	}

	printHostSurvey(opts.out, h, survey)

	if opts.dryRun {
		printHostPlan(opts.out, h, survey)
		fmt.Fprintln(opts.out, "\n--dry-run: nothing was installed, started, or recorded.")
		return nil
	}

	if survey.Uname != "" && survey.Uname != "Linux" {
		return fmt.Errorf("ogre is a Linux x86_64 binary and this box reports %s/%s — aq cannot drive it", survey.Uname, survey.Arch)
	}

	// 2. Install ogre if the box has none. There is no public installer for
	// ogre, so aq will not invent one and will not silently pull a binary from
	// somewhere: the user either has it on the box already or hands aq the one
	// they want installed. Refusing beats guessing at a download.
	if survey.OgrePath == "" {
		if opts.ogreBin == "" {
			return fmt.Errorf("no `ogre` on %s and no --ogre-binary given — install ogre on the box, or re-run with --ogre-binary <path to a Linux x86_64 ogre>", h.SSH)
		}
		if err := checkLocalOgreBinary(opts.ogreBin); err != nil {
			return err
		}
		fmt.Fprintf(opts.out, "\nInstalling %s → %s:%s ...\n", opts.ogreBin, h.SSH, ogreInstallPath)
		if err := opts.upload(h, opts.ogreBin, ogreInstallPath); err != nil {
			return fmt.Errorf("could not install ogre on the box: %w", err)
		}
	}

	// 3. Prove the daemon answers on loopback. This is the whole check that
	// makes the registry entry mean something: ogre's CLI talks to its daemon
	// over 127.0.0.1 and nothing else, so a successful `ogre status` from the
	// box's own shell is a real round trip, not a reachability guess.
	state, reason := probeOgreDaemon(opts.run, h)
	if state != "ok" {
		return fmt.Errorf("ogre is installed on %s but its daemon did not answer on loopback: %s", h.SSH, reason)
	}

	h.AddedAt = time.Now().UTC().Format(time.RFC3339)
	if err := config.PutHost(h); err != nil {
		return err
	}
	hosts, err := config.LoadHosts()
	if err != nil {
		return err
	}
	if err := opts.syncConfig(hosts); err != nil {
		return err
	}

	fmt.Fprintf(opts.out, "\n✓ Registered %s (%s) — ogre answers on loopback.\n", h.Alias, h.SSH)
	fmt.Fprintf(opts.out, "  ssh alias: %s\n", hostAliasFor(h.Alias))
	fmt.Fprintf(opts.out, "  Drive it with `aq ssh host:%s`, `aq run host:%s -- <cmd>`, `aq save host:%s`.\n", h.Alias, h.Alias, h.Alias)
	fmt.Fprintf(opts.out, "  This is detached mode: no Aquanode account is involved and nothing here calls our API.\n")
	fmt.Fprintf(opts.out, "  Want the console, versions, sharing and endpoints too? `aq attach %s`.\n", h.Alias)
	return nil
}

// probeOgreDaemon asks the box's own ogre CLI for status. Three-state on
// purpose: "ok", "unreachable" with the box's own reason, or "unknown" when we
// could not even run the check — never a cheerful default.
func probeOgreDaemon(run remoteRunner, h config.Host) (state, reason string) {
	out, err := run(h, "ogre status --json 2>&1 | tail -20")
	if err != nil {
		return "unknown", strings.TrimSpace(err.Error())
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "unreachable", "`ogre status --json` printed nothing"
	}
	if strings.Contains(text, "{") {
		return "ok", ""
	}
	return "unreachable", firstLine(text)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// printHostSurvey renders what the box told us, marking anything we could not
// determine as unknown rather than leaving it blank and reassuring.
func printHostSurvey(out io.Writer, h config.Host, s hostSurvey) {
	fmt.Fprintf(out, "Box %s (%s)\n", h.Alias, h.SSH)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  OS\t%s\n", orUnknown(strings.TrimSpace(s.Uname+" "+s.Arch)))
	fmt.Fprintf(w, "  GPU\t%s\n", orUnknown(s.GPU))
	fmt.Fprintf(w, "  Workspace\t%s (%s)\n", h.MountPath, existsLabel(s.MountChecked, s.MountExists))
	if s.OgrePath == "" {
		fmt.Fprintf(w, "  ogre\tnot installed\n")
	} else {
		fmt.Fprintf(w, "  ogre\t%s %s\n", s.OgrePath, orUnknown(s.OgreVersion))
		fmt.Fprintf(w, "  ogre daemon\t%s\n", daemonLabel(s.Daemon, s.DaemonReason))
	}
	w.Flush()
}

// printHostPlan states exactly what a real run would change — the dry-run's
// whole job. It names the box as ONE target, because that is what it is.
func printHostPlan(out io.Writer, h config.Host, s hostSurvey) {
	fmt.Fprintln(out, "\nWould:")
	if s.OgrePath == "" {
		fmt.Fprintf(out, "  • install ogre at %s (needs --ogre-binary; aq will not download one)\n", ogreInstallPath)
	} else {
		fmt.Fprintf(out, "  • leave the existing ogre at %s alone\n", s.OgrePath)
	}
	fmt.Fprintf(out, "  • verify ogre's daemon answers on the box's own loopback interface\n")
	fmt.Fprintf(out, "  • record %q in %s\n", h.Alias, hostsPathForDisplay())
	fmt.Fprintf(out, "  • add one `%s` stanza to ~/.ssh/%s (included from your ~/.ssh/config)\n", hostAliasFor(h.Alias), managedHostsConfigName)
	fmt.Fprintln(out, "\nWould NOT: contact the Aquanode API, change your box's ssh keys, or start any workload.")
	fmt.Fprintln(out, "\nThe whole box is one target. Aquanode cannot split a multi-GPU box into")
	fmt.Fprintln(out, "several independent setups — one box runs one setup at a time.")
}

func hostsPathForDisplay() string {
	if p, err := config.HostsPath(); err == nil {
		return p
	}
	return "your aq config dir"
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return strings.TrimSpace(s)
}

func existsLabel(checked, exists bool) string {
	switch {
	case !checked:
		return "unknown"
	case exists:
		return "exists"
	default:
		return "will be created on first push"
	}
}

func daemonLabel(state, reason string) string {
	switch state {
	case "ok":
		return "answers on loopback"
	case "unreachable":
		return "did not answer — " + reason
	default:
		if strings.TrimSpace(reason) == "" {
			return "unknown — aq could not check"
		}
		return "unknown — " + reason
	}
}

// runHostLs lists the registry. It reads nothing but the local file: listing
// detached boxes must work with no login, no network, and no box powered on.
func runHostLs(opts hostOptions) error {
	hosts, err := config.LoadHosts()
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		fmt.Fprintln(opts.out, "No detached hosts registered. Add one with `aq host add <alias> --ssh root@<ip>`.")
		return nil
	}
	w := tabwriter.NewWriter(opts.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tSSH\tWORKSPACE\tMODE\tSSH ALIAS")
	for _, h := range hosts {
		mode := "detached"
		if h.Attached() {
			mode = "attached #" + strconv.Itoa(h.DeploymentID)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h.Alias, h.SSH, orUnknown(h.MountPath), mode, hostAliasFor(h.Alias))
	}
	return w.Flush()
}

// runHostRm drops a registry entry and its ssh stanza.
//
// It never touches the box. Removing a host from the registry is aq forgetting
// a machine, not aq doing anything to a machine — and on hardware the user
// leases those two must never be the same operation.
func runHostRm(opts hostOptions) error {
	if opts.alias == "" {
		return errors.New("an alias is required — usage: aq host rm <alias>")
	}
	h, found, err := config.FindHost(opts.alias)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no host %q in your registry — see `aq host ls`", opts.alias)
	}
	if h.Attached() {
		return fmt.Errorf("host %q is attached as deployment #%d — run `aq release %s` first, which revokes its credentials and leaves the box running", h.Alias, h.DeploymentID, h.Alias)
	}
	if _, err := config.RemoveHost(opts.alias); err != nil {
		return err
	}
	hosts, err := config.LoadHosts()
	if err != nil {
		return err
	}
	if err := opts.syncConfig(hosts); err != nil {
		return err
	}
	fmt.Fprintf(opts.out, "✓ Forgot %s. The box itself is untouched and still running.\n", opts.alias)
	return nil
}

// parseSSHTarget splits `user@host[:port]` into an ssh target and a port.
func parseSSHTarget(target string) (sshTarget string, port int, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", 0, errors.New("--ssh must name a login target, e.g. root@1.2.3.4")
	}
	user, hostPart := splitSSHTarget(target)
	if strings.HasPrefix(hostPart, "[") {
		// A bracketed IPv6 literal, optionally with :port after the bracket.
		end := strings.Index(hostPart, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("invalid --ssh %q — an IPv6 literal needs its closing ']'", target)
		}
		rest := hostPart[end+1:]
		hostPart = hostPart[1:end]
		if strings.HasPrefix(rest, ":") {
			if port, err = strconv.Atoi(rest[1:]); err != nil {
				return "", 0, fmt.Errorf("invalid --ssh %q — %q is not a port", target, rest[1:])
			}
		}
	} else if i := strings.LastIndex(hostPart, ":"); i >= 0 && !strings.Contains(hostPart[i+1:], ":") && strings.Count(hostPart, ":") == 1 {
		if port, err = strconv.Atoi(hostPart[i+1:]); err != nil {
			return "", 0, fmt.Errorf("invalid --ssh %q — %q is not a port", target, hostPart[i+1:])
		}
		hostPart = hostPart[:i]
	}
	if hostPart == "" {
		return "", 0, fmt.Errorf("invalid --ssh %q — it names no host", target)
	}
	if port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid --ssh %q — port %d is out of range", target, port)
	}
	return user + "@" + hostPart, port, nil
}

// resolveHostIdentity returns an ABSOLUTE path to the private key for this box.
//
// Absolute, always: OpenSSH resolves `~` from the passwd entry while aq resolves
// it from $HOME, so a tilde'd IdentityFile in a generated stanza points at a key
// that is not there the moment those two disagree (a container, a CI job).
func resolveHostIdentity(identity string) (string, error) {
	if identity == "" {
		id, _, err := sshPaths()
		return id, err
	}
	expanded, err := expandHome(identity)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("--identity %s: %w", identity, err)
	}
	return abs, nil
}

// expandHome resolves a leading `~/` against the user's home directory.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// checkLocalOgreBinary refuses obviously-wrong input before it is uploaded to
// somebody's leased box: a directory, an unreadable file, or an empty one.
func checkLocalOgreBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("--ogre-binary %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("--ogre-binary %s is a directory", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("--ogre-binary %s is empty", path)
	}
	return nil
}

// uploadToHost scp's a local file to the box and makes it executable.
//
// It lands on a temp path first and is moved into place, so a half-transferred
// binary is never briefly on PATH as a working `ogre`.
func uploadToHost(h config.Host, localPath, remotePath string) error {
	dir, err := sshDir()
	if err != nil {
		return err
	}
	tmpPath := remotePath + ".aq-upload"
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(dir, knownHostsName),
	}
	if h.Port > 0 {
		args = append(args, "-P", strconv.Itoa(h.Port))
	}
	if h.Identity != "" {
		args = append(args, "-i", h.Identity, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, localPath, h.SSH+":"+tmpPath)
	cmd := exec.Command("scp", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp to %s: %w", h.SSH, err)
	}
	_, err = runRemoteCapture(h, "chmod 0755 "+shellQuote(tmpPath)+" && mv -f "+shellQuote(tmpPath)+" "+shellQuote(remotePath))
	return err
}
