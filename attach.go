package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// defaultOgrePort is the port an attached box publishes ogre's control API on.
// Only attached boxes ever listen for us: in detached mode ogre's CLI reaches
// its daemon over loopback and nothing dials in from outside at all.
const defaultOgrePort = 8443

// ogreEnvPath is the file `aq attach` writes ogre's managed-mode credentials
// into. It is under a directory aq creates, so nothing else on the box owns it —
// but it is still written inside `# BEGIN aquanode` markers, and a file whose
// existing content sits outside those markers is refused rather than replaced.
// The rule is the same everywhere on a machine we do not own: merge, or refuse.
const ogreEnvPath = "/etc/aquanode/ogre.env"

// ogreLogPath is where the attached daemon's output goes, so a box that stops
// answering has something to read afterwards.
const ogreLogPath = "/var/log/aquanode-ogre.log"

// attachOptions configures runAttach.
type attachOptions struct {
	alias      string
	publicHost string
	ogrePort   int
	dryRun     bool
	yes        bool
	out        io.Writer
	errOut     io.Writer

	client *api.Client
	run    remoteRunner
	now    func() time.Time
}

// attachCmd parses `aq attach <alias>`.
//
// Attach is the second half of detached mode, not a replacement for it: the same
// box, the same artifact, the same verbs — plus the console, version history,
// fork/share, teams, metrics and endpoints, which all need a control plane. A
// box that cannot accept an inbound connection from us stays detached and loses
// none of the first list.
func attachCmd(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	host := fs.String("host", "", "Address the Aquanode orchestrator should dial (default: the box's ssh host)")
	ogrePort := fs.Int("ogre-port", 0, "Port ogre's control API listens on (default: the host's, else "+strconv.Itoa(defaultOgrePort)+")")
	dryRun := fs.Bool("dry-run", false, "Print the plan; write nothing on the box and create nothing in Aquanode")
	yes := fs.Bool("yes", false, "Skip the interactive confirmation")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: aq attach <alias> — the alias of a box from `aq host ls`")
	}

	// A dry run still needs a login: the plan it prints names the account the
	// box would be adopted into, and printing a plan for an account we cannot
	// confirm is the kind of half-truth this whole command is built to avoid.
	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runAttach(attachOptions{
		alias:      positional[0],
		publicHost: strings.TrimSpace(*host),
		ogrePort:   *ogrePort,
		dryRun:     *dryRun,
		yes:        *yes,
		out:        os.Stdout,
		errOut:     os.Stderr,
		client:     newControlClient(cred),
	})
}

func (o attachOptions) withDefaults() attachOptions {
	if o.out == nil {
		o.out = os.Stdout
	}
	if o.errOut == nil {
		o.errOut = os.Stderr
	}
	if o.run == nil {
		o.run = runRemoteCapture
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

// portState is FREE / BUSY / UNKNOWN, kept as three genuinely distinct values
// rather than a pair of booleans. "Unknown" itself has two different causes and
// they must never render the same warning: portNoTool means the box has
// neither ss nor netstat (a fact about the box), portUnobserved means the
// preflight's "port=" line was never seen at all — the script ran but its
// signal was lost in transit (e.g. glued to the line before it; see
// attachPreflightScript). Collapsing the second into the first told the user
// "no ss or netstat on the box" on boxes that had ss and reported busy —
// exactly the one case attach exists to warn about.
type portState int

const (
	portUnobserved portState = iota // zero value: no "port=" line was ever parsed
	portNoTool                      // box has neither ss nor netstat
	portFree
	portBusy
)

// attachPreflight is what the box told us before anything was created anywhere.
type attachPreflight struct {
	survey hostSurvey
	// authorizedKeys is the current content of the login's authorized_keys.
	// It is read — not to be written back, but because a preflight that cannot
	// READ what a later step will modify must refuse. "Could not look" blocks.
	authorizedKeys string
	// port is FREE / BUSY / one of two UNKNOWN causes — see portState.
	port portState
}

// attachPreflightScript reads. It writes nothing, starts nothing, and installs
// nothing — a box surveyed by a dry run is byte-identical afterwards.
func attachPreflightScript(mountPath string, ogrePort int) string {
	port := strconv.Itoa(ogrePort)
	return surveyScript(mountPath) + strings.Join([]string{
		`ak="$HOME/.ssh/authorized_keys"`,
		`if [ ! -e "$ak" ]; then echo "ak=absent"; elif [ -r "$ak" ]; then echo "ak=readable"; else echo "ak=unreadable"; fi`,
		`echo "ak_begin"`,
		`[ -r "$ak" ] && cat "$ak"`,
		// `cat` does not guarantee a trailing newline — an authorized_keys file
		// written by cloud-init/terraform commonly ends without one. Without this
		// printf, "ak_end" would land glued onto the last key line as one line
		// (e.g. "...user@hostak_end"), the parser's ak_end marker would never
		// match, inKeys would never turn back off, and every line after it —
		// including "port=busy" — would be silently swallowed into the key
		// content instead of parsed — this was seen live, on boxes that had
		// ss, as the port probe collapsing to "no ss or netstat". The leading
		// newline forces "ak_end" onto its own line regardless of whether the
		// file the box holds ends in one.
		`printf '\nak_end\n'`,
		`if command -v ss >/dev/null 2>&1; then`,
		`  if ss -lnt 2>/dev/null | grep -q ":` + port + ` "; then echo "port=busy"; else echo "port=free"; fi`,
		`elif command -v netstat >/dev/null 2>&1; then`,
		`  if netstat -lnt 2>/dev/null | grep -q ":` + port + ` "; then echo "port=busy"; else echo "port=free"; fi`,
		`else echo "port=notool"; fi`,
		`echo "preflight_ok=1"`,
	}, "\n") + "\n"
}

// parseAttachPreflight decodes the preflight output.
//
// It refuses on anything it could not establish about authorized_keys. That
// file is the customer's only way onto a machine they hold on a multi-year lease
// and that we cannot restore access to; an attach that cannot read it has no
// business writing anywhere near it.
func parseAttachPreflight(raw []byte) (attachPreflight, error) {
	survey, err := parseSurvey(raw)
	if err != nil {
		return attachPreflight{}, err
	}
	p := attachPreflight{survey: survey}

	text := string(raw)
	if !strings.Contains(text, "preflight_ok=1") {
		return attachPreflight{}, errors.New("the box did not complete the attach preflight — aq could not read enough to proceed safely")
	}

	akState := ""
	inKeys := false
	var keys strings.Builder
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		switch strings.TrimSpace(trimmed) {
		case "ak_begin":
			inKeys = true
			continue
		case "ak_end":
			inKeys = false
			continue
		}
		if inKeys {
			keys.WriteString(trimmed)
			keys.WriteString("\n")
			continue
		}
		key, value, found := strings.Cut(strings.TrimSpace(trimmed), "=")
		if !found {
			continue
		}
		switch key {
		case "ak":
			akState = value
		case "port":
			switch value {
			case "free":
				p.port = portFree
			case "busy":
				p.port = portBusy
			case "notool":
				p.port = portNoTool
				// Anything else (including a value we do not recognize) leaves
				// p.port at its zero value, portUnobserved — loud UNKNOWN, never
				// laundered into "no tool".
			}
		}
	}

	switch akState {
	case "readable":
		p.authorizedKeys = keys.String()
	case "absent":
		p.authorizedKeys = ""
	default:
		return attachPreflight{}, errors.New("could not read the box's ~/.ssh/authorized_keys — refusing to attach. " +
			"That file is how you get onto this machine, and aq will not write anywhere near a box whose access it cannot first read")
	}
	return p, nil
}

// runAttach walks the attach contract: preflight → adopt → redeem → configure →
// activate. Nothing is recorded locally until the orchestrator's own probe has
// completed a real round-trip to the box.
func runAttach(opts attachOptions) error {
	opts = opts.withDefaults()

	h, err := lookupHost(opts.alias)
	if err != nil {
		return err
	}
	if h.Attached() {
		return fmt.Errorf("%s is already attached as deployment #%d — release it first with `aq release %s`", h.Alias, h.DeploymentID, h.Alias)
	}

	ogrePort := opts.ogrePort
	if ogrePort == 0 {
		ogrePort = h.OgrePort
	}
	if ogrePort == 0 {
		ogrePort = defaultOgrePort
	}
	publicHost := opts.publicHost
	if publicHost == "" {
		_, publicHost = splitSSHTarget(h.SSH)
	}
	mountPath := h.MountPath
	if mountPath == "" {
		mountPath = defaultHostMountPath
	}

	raw, err := opts.run(h, attachPreflightScript(mountPath, ogrePort))
	if err != nil {
		return fmt.Errorf("could not reach %s over ssh: %w", h.SSH, err)
	}
	pre, err := parseAttachPreflight(raw)
	if err != nil {
		return err
	}
	if pre.survey.OgrePath != "" {
		pre.survey.Daemon, pre.survey.DaemonReason = probeOgreDaemon(opts.run, h)
	}
	if pre.survey.OgrePath == "" {
		return fmt.Errorf("no `ogre` on %s — run `aq host add`/install ogre there first; attach configures ogre, it does not install it", h.SSH)
	}

	printAttachPlan(opts.out, h, pre, publicHost, ogrePort, mountPath)

	if opts.dryRun {
		fmt.Fprintln(opts.out, "\n--dry-run: nothing was written on the box and nothing was created in Aquanode.")
		return nil
	}

	if !opts.yes {
		if !isInteractiveStdin() {
			return errors.New("refusing to write to a box you lease without confirmation in a non-interactive shell — re-run with --yes")
		}
		fmt.Fprint(opts.out, "\nAttach this box to your Aquanode account? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			return errors.New("attach cancelled")
		}
	}

	adopted, err := opts.client.AdoptExternal(api.AdoptExternalRequest{
		Name:      h.Alias,
		Host:      publicHost,
		OgrePort:  ogrePort,
		GPU:       pre.survey.GPU,
		MountPath: mountPath,
	})
	if err != nil {
		return fmt.Errorf("could not register the box with Aquanode: %w", err)
	}
	fmt.Fprintf(opts.out, "\nRegistered as deployment #%d.\n", adopted.DeploymentID)

	// Recorded now, before anything that can fail — not only after the probe
	// succeeds. A refused attach used to say "release it with `aq release
	// <alias>`" while `aq release` itself required h.Attached() (DeploymentID +
	// AttachedAt, set only on success), so a failed activation, redeem or box
	// configuration left a PROVISIONING row the local CLI had never heard of:
	// the exact command the refusal printed answered "not attached — nothing to
	// release". PendingDeploymentID is what `aq release` needs
	// to find that row even though attach never finished; it does not make
	// h.Attached() true, and it is cleared the moment attach actually succeeds.
	pending := h
	pending.PendingDeploymentID = adopted.DeploymentID
	if err := config.PutHost(pending); err != nil {
		return fmt.Errorf("registered as deployment #%d but could not record it locally: %w\n"+
			"the box is NOT attached; clear the row with `POST /deployments/%d/release` or contact support", adopted.DeploymentID, err, adopted.DeploymentID)
	}

	port := adopted.OgrePort
	if port == 0 {
		port = ogrePort
	}

	install, err := opts.client.RedeemExternalInstallConfig(adopted.DeploymentID, adopted.InstallToken)
	if err != nil {
		return fmt.Errorf("could not redeem the install token for deployment #%d: %w", adopted.DeploymentID, err)
	}
	if install.OgrePort > 0 {
		port = install.OgrePort
	}

	if err := configureOgreOnBox(opts, h, install, adopted.DeploymentID, port); err != nil {
		return err
	}

	// The probe is the gate. A box is recorded as attached only after the
	// orchestrator itself completed a round-trip to it — never on the strength
	// of a request that was merely accepted, and never on this CLI's own
	// ability to reach the box, which proves a different thing entirely.
	res, err := opts.client.ActivateExternal(adopted.DeploymentID)
	if err != nil {
		var unreachable *api.ExternalUnreachableError
		if errors.As(err, &unreachable) {
			return fmt.Errorf("Aquanode could not reach %s:%d — the box is NOT attached.\n"+
				"  reason: %s\n"+
				"  Deployment #%d stays PROVISIONING and the probe failure is recorded; release it with `aq release %s`.\n"+
				"  Open inbound TCP %d from the internet and re-run, or stay in detached mode — it needs no inbound connectivity at all\n"+
				"  and keeps capture, restore, setups, run, logs, ssh and sync exactly as they are.\n"+
				"  If this box is on a container-pool marketplace listing (simplepod, vast.ai and similar), this is\n"+
				"  likely why: attach needs the port ogre listens on and the port we dial to be the SAME port, and a\n"+
				"  port-mapped box remaps them — so port %d reaching this box from the internet is never possible no\n"+
				"  matter which --ogre-port you pass. Attach only works on a box with a real public IP and a direct\n"+
				"  inbound path (bare metal, most VM-pool providers) — detached mode has no such requirement.",
				publicHost, port, unreachable.Reason, adopted.DeploymentID, h.Alias, port, port)
		}
		return fmt.Errorf("could not activate deployment #%d: %w", adopted.DeploymentID, err)
	}

	h.DeploymentID = adopted.DeploymentID
	h.PublicHost = publicHost
	h.OgrePort = port
	h.AttachedAt = opts.now().UTC().Format(time.RFC3339)
	h.PendingDeploymentID = 0 // attach succeeded — no longer just pending
	if err := config.PutHost(h); err != nil {
		return err
	}

	fmt.Fprintf(opts.out, "\n✓ %s is attached as deployment #%d (%s).\n", h.Alias, adopted.DeploymentID, orUnknown(res.Status))
	fmt.Fprintf(opts.out, "  Aquanode completed a round-trip to %s:%d.\n", publicHost, port)
	fmt.Fprintf(opts.out, "  The box bills nothing: we did not rent it and never will.\n")
	fmt.Fprintf(opts.out, "  Hand it back any time with `aq release %s` — that revokes our credentials and drops the row.\n", h.Alias)
	fmt.Fprintf(opts.out, "  The box keeps running, and no provider is ever contacted.\n")
	return nil
}

// printAttachPlan is the survey-show-confirm half of the posture `aq import`
// established: the user sees exactly what will change on a machine they lease
// before any of it happens.
func printAttachPlan(out io.Writer, h config.Host, pre attachPreflight, publicHost string, ogrePort int, mountPath string) {
	printHostSurvey(out, h, pre.survey)

	fmt.Fprintln(out, "\nWould, on the box:")
	fmt.Fprintf(out, "  • create %s (0600) holding ogre's credentials, inside `%s` markers\n", ogreEnvPath, beginMarker)
	fmt.Fprintf(out, "  • restart ogre's daemon on port %d, logging to %s\n", ogrePort, ogreLogPath)
	fmt.Fprintln(out, "\nWould NOT, on the box:")
	fmt.Fprintf(out, "  • touch ~/.ssh/authorized_keys outside `%s` markers — read OK, %s\n", beginMarker, authorizedKeysSummary(pre.authorizedKeys))
	fmt.Fprintln(out, "  • change any file's content outside those markers, install packages, or stop your workload")

	fmt.Fprintln(out, "\nWould, in Aquanode:")
	fmt.Fprintf(out, "  • create a deployment named %q at %s:%d\n", h.Alias, publicHost, ogrePort)
	fmt.Fprintf(out, "  • workspace %s; idle auto-pause OFF (your lease is already paid for)\n", mountPath)
	fmt.Fprintln(out, "  • bill nothing for the hardware — we did not rent it")
	fmt.Fprintln(out, "  • probe the box from our infrastructure, and refuse to attach it if that fails")

	switch pre.port {
	case portBusy:
		fmt.Fprintf(out, "\nWarning: something is already listening on port %d. Pass --ogre-port to pick another.\n", ogrePort)
	case portNoTool:
		fmt.Fprintf(out, "\nNote: aq could not check whether port %d is free (no ss or netstat on the box).\n", ogrePort)
	case portUnobserved:
		fmt.Fprintf(out, "\nNote: aq could not read the box's port check — treat port %d as unknown. "+
			"If ogre refuses to start, something else already owns it; re-run with --ogre-port to pick another.\n", ogrePort)
	case portFree:
		// Nothing to warn about.
	}

	// Section K, stated up front rather than discovered later by whoever paid
	// for eight GPUs and expected to hand three of them to three people.
	fmt.Fprintln(out, "\nOne box is one deployment running one setup at a time. Aquanode cannot")
	fmt.Fprintln(out, "partition a multi-GPU box into several independent setups — the whole box")
	fmt.Fprintln(out, "attaches as a single target. That capability does not exist in either mode.")
}

// authorizedKeysSummary describes the customer's key file without printing it.
// The count and the fact that it was readable are the whole point; the keys
// themselves are theirs and do not belong in our terminal output.
func authorizedKeysSummary(content string) string {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			n++
		}
	}
	if n == 0 {
		return "no existing keys"
	}
	return fmt.Sprintf("%d existing key(s) will be left byte-identical", n)
}

// configureOgreOnBox writes the credential env file and restarts the daemon.
//
// The env file is merged into a marker region rather than overwritten, and the
// current content is read first: if it exists and holds anything outside our
// markers, aq refuses rather than replacing something it did not write.
func configureOgreOnBox(opts attachOptions, h config.Host, install *api.ExternalInstallConfig, deploymentID, port int) error {
	existing, err := opts.run(h, "if [ -e "+shellQuote(ogreEnvPath)+" ]; then if [ -r "+shellQuote(ogreEnvPath)+" ]; then cat "+shellQuote(ogreEnvPath)+"; echo '__AQ_READ_OK__'; else echo '__AQ_UNREADABLE__'; fi; else echo '__AQ_ABSENT__'; fi")
	if err != nil {
		return fmt.Errorf("could not read %s on the box: %w", ogreEnvPath, err)
	}
	text := string(existing)
	switch {
	case strings.Contains(text, "__AQ_ABSENT__"):
		text = ""
	case strings.Contains(text, "__AQ_READ_OK__"):
		text = strings.TrimSuffix(text, "__AQ_READ_OK__\n")
	default:
		return fmt.Errorf("%s exists on %s but aq cannot read it — refusing to overwrite a file it could not first look at", ogreEnvPath, h.SSH)
	}

	body := renderOgreEnv(install, deploymentID, port)
	merged, err := applyMarkerRegion(text, body)
	if err != nil {
		return fmt.Errorf("%s on %s: %w", ogreEnvPath, h.SSH, err)
	}
	if strings.Contains(merged, ogreEnvHeredocTag) {
		return fmt.Errorf("refusing to write %s: its content contains the transfer delimiter", ogreEnvPath)
	}

	fmt.Fprintf(opts.out, "Writing %s and restarting ogre on port %d...\n", ogreEnvPath, port)
	if _, err := opts.run(h, writeOgreEnvScript(merged)); err != nil {
		return fmt.Errorf("could not write %s on the box: %w", ogreEnvPath, err)
	}
	if _, err := opts.run(h, restartOgreScript(port)); err != nil {
		return fmt.Errorf("could not start ogre on the box: %w", err)
	}
	return nil
}

// ogreEnvHeredocTag delimits the env file during transfer. Quoted in the script
// so the shell performs no expansion on the credential material passing through.
const ogreEnvHeredocTag = "AQ_OGRE_ENV_EOF"

// renderOgreEnv builds the env body ogre reads its managed-mode configuration
// from. Cert and key are base64 because they are multi-line PEM and every path
// they travel — a heredoc here, a shell setup script on the provisioned path —
// mangles raw newlines; ogre's own config decodes exactly this encoding.
func renderOgreEnv(install *api.ExternalInstallConfig, deploymentID, port int) string {
	var b strings.Builder
	b.WriteString("JWT_SECRET=" + shellQuote(install.OgreJWTSecret) + "\n")
	b.WriteString("OGRE_PROXY_PASSWORD=" + shellQuote(install.OgreProxyPass) + "\n")
	b.WriteString("OGRE_TLS_CERT=" + shellQuote(base64.StdEncoding.EncodeToString([]byte(install.TLSCertPEM))) + "\n")
	b.WriteString("OGRE_TLS_KEY=" + shellQuote(base64.StdEncoding.EncodeToString([]byte(install.TLSKeyPEM))) + "\n")
	b.WriteString("OGRE_PORT=" + strconv.Itoa(port) + "\n")
	b.WriteString("AQUANODE_DEPLOYMENT_ID=" + strconv.Itoa(deploymentID) + "\n")
	if install.OrchestratorURL != "" {
		b.WriteString("AQUANODE_ORCHESTRATOR_URL=" + shellQuote(install.OrchestratorURL) + "\n")
	}
	return b.String()
}

// writeOgreEnvScript writes the merged file via a temp file + mv, so a
// half-written credential file is never briefly readable as a whole one.
func writeOgreEnvScript(content string) string {
	tmp := ogreEnvPath + ".aq-tmp"
	return "set -e\n" +
		"mkdir -p " + shellQuote("/etc/aquanode") + "\n" +
		"umask 077\n" +
		"cat > " + shellQuote(tmp) + " <<'" + ogreEnvHeredocTag + "'\n" +
		content +
		ogreEnvHeredocTag + "\n" +
		"chmod 0600 " + shellQuote(tmp) + "\n" +
		"mv -f " + shellQuote(tmp) + " " + shellQuote(ogreEnvPath) + "\n"
}

// restartOgreScript starts ogre in managed mode with the credentials just
// written. It stops only a daemon started this same way — the pattern is aq's
// own invocation, not every process with "ogre" in its name.
//
// THE PATTERN IS ASSEMBLED ON THE BOX, FROM A VARIABLE, AND NEVER APPEARS
// LITERALLY IN THIS SCRIPT. `ssh host '<script>'` hands the whole script to the
// remote shell as one argv element, so the shell's own /proc/<pid>/cmdline
// CONTAINS the script text. With the pattern spelled out, `pkill -f 'ogre -port
// 8443'` matched that shell and killed the very session running the attach:
// ssh died with status 255 before ogre was ever started, on every box, every
// time. `pgrep -f` had the mirror-image bug — it matched the same shell and so
// reported "ogre stayed up" whether or not anything had started, which is the
// worse half: a false green on the only check that the daemon is alive.
//
// Building the pattern from $AQ_OGRE_PORT keeps the literal out of the script
// text, and skipping $$ makes the self-exclusion explicit rather than a
// property of how the string happens to be spelled.
func restartOgreScript(port int) string {
	p := strconv.Itoa(port)
	return "set -a\n" +
		". " + shellQuote(ogreEnvPath) + "\n" +
		"set +a\n" +
		"AQ_OGRE_PORT=" + p + "\n" +
		`AQ_OGRE_PAT="ogre -port ${AQ_OGRE_PORT}"` + "\n" +
		// Kill previous daemons, never this shell or its ancestors.
		"for pid in $(pgrep -f -- \"$AQ_OGRE_PAT\" 2>/dev/null); do\n" +
		"  [ \"$pid\" = \"$$\" ] && continue\n" +
		"  kill \"$pid\" >/dev/null 2>&1 || true\n" +
		"done\n" +
		"touch " + shellQuote(ogreLogPath) + " 2>/dev/null || true\n" +
		"nohup ogre -port \"$AQ_OGRE_PORT\" >> " + shellQuote(ogreLogPath) + " 2>&1 &\n" +
		"sleep 1\n" +
		"AQ_OGRE_UP=0\n" +
		"for pid in $(pgrep -f -- \"$AQ_OGRE_PAT\" 2>/dev/null); do\n" +
		"  [ \"$pid\" = \"$$\" ] && continue\n" +
		"  AQ_OGRE_UP=1\n" +
		"done\n" +
		"[ \"$AQ_OGRE_UP\" = 1 ] || { echo 'ogre did not stay up — see " + ogreLogPath + " on the box' >&2; exit 1; }\n"
}
