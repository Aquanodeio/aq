// Command aq is the Aquanode control / funnel CLI.
//
// It runs on a developer's laptop and talks to the Aquanode API to rent GPUs,
// provision the ogre on-box agent, and restore snapshots. It is a thin
// orchestration wrapper over `ogre` (the OSS on-box agent) + the Aquanode API —
// it does not reimplement ogre.
//
// Subcommands are built out by the funnel tickets. `login` (device-grant
// pairing) ships here; deploy / up follow. See research/action/02-console-dx.md
// in the meta-repo.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// version is overridden at build time via -ldflags "-X main.version=...", which is
// how release binaries get their number. `go install github.com/Aquanodeio/aq@latest`
// sets no ldflags, so those builds fall back to the version the Go toolchain stamps
// into the build info from the module tag.
var version = "0.0.0-dev"

// resolveVersion returns the ldflags-injected version when there is one, else the
// module version recorded in the build info. Returns the dev sentinel for a plain
// `go build` from a checkout, where neither source has a real version.
func resolveVersion() string {
	if version != "0.0.0-dev" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok || bi.Main.Version == "" {
		return version
	}
	// A `go build` from a checkout reports "(devel)"; only a module-proxy install
	// carries a real tag.
	if bi.Main.Version == "(devel)" {
		return version
	}
	return strings.TrimPrefix(bi.Main.Version, "v")
}

func main() {
	// Label every API request `aq/<version>` so the orchestrator can tell a CLI
	// action from a scripted one. Set here rather than in the api package so the
	// ldflags-injected version stays a main-package concern.
	version = resolveVersion()
	api.Version = version

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd, args := os.Args[1], os.Args[2:]

	// Resolve the host this run will talk to ONCE, here, and apply the two
	// target rails before anything dispatches — see prodguard.go for why they
	// live at the dispatch rather than inside each command. Doing it here also
	// keeps the rails out of the run<Verb> functions the tests drive directly,
	// so no existing test's captured output changes.
	//
	// A credential that fails to load is not an error at this point: the
	// commands that need one report that themselves, with a message about
	// logging in. Here it only narrows resolveAPIURL to the env var and the
	// built-in default, which is the same host the failing command would have
	// used anyway.
	cred, _ := config.Load()
	apiURL := resolveAPIURL(cred)

	if _, billable := billableCommands[cmd]; billable {
		var prodFlag bool
		args, prodFlag = stripProdFlag(args)
		allowProd := prodFlag || os.Getenv("AQ_ALLOW_PROD") == "1"
		overseen := hasHumanOversight(os.Getenv, isInteractiveStdin())
		if err := guardBillable(cmd, apiURL, args, allowProd, overseen); err != nil {
			run(err)
		}
	}
	announceTarget(cmd, apiURL, os.Stderr)

	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("aq %s\n", version)
	case "help", "--help", "-h":
		usage()
	case "gpus":
		run(gpus(args))
	case "login":
		run(login(args))
	case "up":
		run(up(args))
	case "deploy":
		run(deploy(args))
	case "import":
		run(importCmd(args))
	case "host":
		run(hostCmd(args))
	case "attach":
		run(attachCmd(args))
	case "release":
		run(releaseCmd(args))
	case "ssh":
		run(sshCmd(args))
	case "push":
		run(push(args))
	case "run":
		run(runCmd(args))
	case "logs":
		run(logsCmd(args))
	case "ls":
		run(lsCmd(args))
	case "status":
		run(status(args))
	case "save":
		run(snapshot(args))
	case "share":
		run(share(args))
	case "fork":
		run(fork(args))
	case "edit-version":
		run(editVersion(args))
	case "pause":
		run(pause(args))
	case "autopause":
		run(autopause(args))
	case "force-detach":
		run(forceDetach(args))
	case "sync-now":
		run(syncNow(args))
	case "setups":
		run(setups(args))
	case "idle":
		run(idle(args))
	case "endpoint":
		run(endpoint(args))
	case "call":
		run(call(args))
	case "calls":
		run(calls(args))
	case "down":
		run(down(args))
	case "logout":
		run(logout(args))
	case "whoami":
		run(whoami(args))
	default:
		fmt.Fprintf(os.Stderr, "aq: unknown command %q\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

// run reports a command error to stderr and exits non-zero.
func run(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "aq: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `aq: Aquanode control CLI

Usage:
  aq <command> [flags]

Commands:
  gpus          Browse live GPU offers across every provider (no account needed)
  login         Pair this CLI to your Aquanode account (device login)
  up            Rent the cheapest matching GPU and bring up a working setup
  deploy        Restore a save onto a freshly-rented Aquanode GPU box
  import        Capture a box you rent elsewhere into a new Aquanode setup
  host          Register a box you own or lease, and drive it with no account
  attach        Adopt a registered box into your Aquanode control plane
  release       Hand an attached box back. The box keeps running
  ssh           Open a shell on a setup (managed key + ~/.ssh/config alias)
  push          Send your working directory to a box you already rented
  run           Push the working directory, then run a command on the box
  logs          Read a detached run's output
  ls            List your deployments: what is running and what it costs
  status        Show a setup's status, HTTPS URL, and credentials
  save          Save a setup's current state into its named lineage
  share         Get a link to one saved version of a setup
  fork          Turn a share link into a new setup in your own library
  edit-version  Edit a saved version's label, description, or visibility
  pause         Save a setup, then release its machine (resume later with up)
  autopause     Turn a setup's auto-pause-when-idle preference on or off
  force-detach  Break a setup's lease even mid-sync (can lose unsynced work)
  sync-now      Force a setup's sync tick right now
  setups        List the setups you own
  idle          View or change a DEPLOYMENT's idle-auto-pause thresholds
  endpoint      Make a setup version callable, repoint it, or remove it
  call          Make a call against an endpoint
  calls         List an endpoint's recent calls
  down          Tear down a setup (stop the rented GPU box)
  logout        Remove the stored CLI credential
  whoami        Show the current login state
  version       Print the aq version
  help          Show this help

gpus:
  aq gpus                     Browse every live GPU offer across all providers,
                              cheapest first. Works with no account: nothing
                              is read from or written to ~/.config/aq.

  --gpu <model>       Filter to a GPU model (substring, case-insensitive,
                      e.g. "B200")
  --max-price <n>     Only show offers at or below this hourly price
                      (the WHOLE offer's price, not per-GPU)
  --provider <name>   Restrict to a single provider (e.g. runpod)
  --region <name>     Filter to a region (substring, case-insensitive)
  --limit <n>         Max rows to print (default 20, 0 = all). With --json,
                      every match is returned unless you pass this explicitly
  --json              Print the filtered offers as JSON instead of a table

up flags:
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --gpus <n>         How many GPUs the box should have (default 1, max 8).
                     Only offers with at least this many are considered.
  --max-price <n>    Only rent GPUs at or below this hourly price
                     (the WHOLE offer's price, not per-GPU)
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)
  --auto-pause       Enable idle auto-pause on this deployment (off by default)
  --warn-after <duration>  With --auto-pause: warn after this much idle time
  --pause-after <duration> With --auto-pause: auto-pause after this much idle time

  App (optional, you get a bare GPU box if you pick neither):
  --comfyui          Also install ComfyUI
  --jupyter          Also install Torch + Jupyter instead

deploy flags:
  --snapshot <id>    Save to deploy (id from aq / the console, e.g. ext-42)
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --gpus <n>         How many GPUs the box should have (default 1, max 8).
                     Only offers with at least this many are considered.
  --max-price <n>    Only rent GPUs at or below this hourly price
                     (the WHOLE offer's price, not per-GPU)
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)

  App (optional, relaunches ComfyUI by default; --no-app restores data only):
  --comfyui          Relaunch ComfyUI on the restored data
  --jupyter          Relaunch Torch + Jupyter on the restored data instead
  --no-app           Restore only, do not relaunch an app

import:
  Run ON a box you already rent somewhere else (RunPod, Vast, your own
  hardware). Captures its environment into a new Aquanode setup, so it can
  be launched on any provider we support. Survey-first: aq shows exactly what
  it will and won't capture, and asks before anything is uploaded.

  aq import                 Survey, confirm, capture, and register the setup
  aq import --dry-run       Survey and print the plan; capture/upload nothing
  aq import --include <path>  Add a path to capture (repeatable)
  aq import --exclude <path>  Drop a detected path from capture (repeatable)
  aq import --name <name>   Name the resulting setup (default: from hostname)
  aq import --yes           Skip the interactive confirmation
  aq import --launch [--gpu <model>] [--max-price <n>] [--provider <name>]
                             After import, rent a GPU and restore onto it
                             (billable). Prints the install-preview verdict:
                             template, suggested hardware, compatibility
                             warnings, before anything is rented. Defaults
                             the GPU to the one observed on the source box.
  aq import --resume <setup-id>
                             Resume an import that started but didn't finish
                             (e.g. the upload credentials expired mid-capture).
                             Re-mints write credentials and re-runs the
                             capture into the exact same storage location:
                             restic dedups what already landed, so this never
                             restarts from zero and never bills a second,
                             parallel setup for the same box.

host / attach / release (boxes we never provisioned):
  Two modes for a machine you already own or lease, sharing one artifact format.

  DETACHED: your box, no control plane, no Aquanode account required. aq drives
  ogre on the box over your own ssh session, and ogre's CLI reaches its daemon on
  loopback, so the box needs no inbound connectivity from us at all. Nothing in
  detached mode contacts the Aquanode API.

  aq host add <alias> --ssh root@1.2.3.4
                             Survey the box, verify ogre's daemon answers on
                             loopback, and register it locally. Survey-first:
                             you see what aq found before anything changes.
  aq host add … --dry-run    Survey and print the plan; write nothing, anywhere
  aq host ls                 List registered boxes
  aq host rm <alias>         Forget a box. The box itself is untouched.

  --identity <path>    Private key to authenticate with (default: aq's own)
  --mount-path <dir>   Workspace root on the box (default: /workspace)
  --ogre-port <n>      Port ogre listens on once attached (default: 8443)
  --ogre-binary <path> Upload this Linux x86_64 ogre when the box has none.
                       There is no public ogre installer, so aq will not
                       download one: it installs the binary you name, or
                       refuses.

  Then address the box as "host:<alias>" from any box-facing verb:
    aq ssh host:lease-a              aq push host:lease-a
    aq run host:lease-a -- nvidia-smi    aq logs host:lease-a
    aq status host:lease-a           (ogre status, read from the box)
    aq save host:lease-a             (ogre snapshot, into your own bucket)
    aq sync-now host:lease-a         (ogre push, to your configured remote)
    aq up host:lease-a               (bring services up in place; rents nothing)

  ATTACHED: your box, our control plane. The box becomes a deployment we never
  provisioned and gains the console, version history, fork/share, teams, metrics
  and endpoints.

  aq attach <alias>          Adopt a registered box (needs a login)
  aq attach <alias> --dry-run
                             Print the plan; write nothing on the box and
                             create nothing in Aquanode
  aq attach <alias> --yes    Skip the confirmation
  --host <addr>              Address our orchestrator should dial
                             (default: the box's ssh host)

  Attach reaches the box one way only: a public address, the port open inbound
  from our infrastructure, TLS pinned. It probes before it commits, and a box it
  cannot reach is NOT attached: the failure is reported with the probe's own
  reason and the box stays fully usable in detached mode.

  Attach requires ogre's listen port and the port we dial to be THE SAME port:
  there is no separate dial port. On a port-mapped box (most container-pool
  marketplace listings: simplepod, vast.ai and similar, where sshd and the
  workload get remapped external ports and 8443 inbound does not reach the same
  8443 the box listens on) that equality can never hold, so attach cannot work
  there no matter which port you pass. This is a direct-connectivity-only
  design choice, not a bug: it is scoped to boxes with a real public IP and an
  inbound path to it (bare metal, most VM-pool providers). Detached mode has no
  such requirement: it needs no inbound connectivity at all.

  Everything aq writes on your box goes inside "# BEGIN aquanode" markers, and
  aq refuses to write to any file it could not first read. Your existing
  authorized_keys is never replaced.

  One attached box is ONE deployment running ONE setup at a time. Aquanode
  cannot partition a multi-GPU box into several independent setups: the whole
  box attaches as a single target. That does not exist in either mode.

  aq release <alias>         Hand an attached box back: Aquanode revokes its
                             credentials and drops its deployment row. The box
                             KEEPS RUNNING and no provider is ever contacted:
                             this is not a terminate. (--keep-host keeps the
                             box in your registry for detached use.)

  Detached does: capture, restore, setups, run/logs/ssh/sync, ogre up
  templates, BYO bucket.
  Attached adds: teams and RBAC, share/fork, the console, endpoints and
  aq call, cross-provider burst, the marketplace.
  Neither does: splitting one box across several independent setups.

ssh:
  aq ssh                     Open a shell on your only live deployment
  aq ssh <name|id>           Open a shell on a deployment by --name or id
  aq ssh <name> -- <cmd…>    Run a command on the box instead of opening a shell

  --print            Print the ssh command that would run, and exit
  -L <spec>          Forward a local port, e.g. 8888:localhost:8888 (repeatable)
  --user <name>      Override the login user (default: root)

  aq manages ~/.ssh/aquanode.config (included from your ~/.ssh/config) with one
  "aq-<name>" alias per live box, so ssh, scp, rsync, and VSCode Remote-SSH all
  work with that alias and no aq involved. If you have no SSH key at all, aq
  generates a passphrase-less one at ~/.ssh/aquanode_ed25519.

push / run:
  The local-code loop: edit on your laptop, execute on the GPU. Both send a
  directory tree over the same managed "aq-<name>" alias ssh uses, nothing
  new to authenticate, and scp/rsync against that alias keep working too.

  aq push [name|id]          Send the current directory to /workspace
  aq run [name|id] -- <cmd>  Send it, then run <cmd> in it with the terminal
                              attached (Ctrl-C reaches the remote process)

  --from <dir>       Local directory to send (default: the current directory)
  --to <dir>         Destination on the box (default: /workspace, absolute)
  --exclude <pat>    Skip paths matching this pattern (repeatable)
  --no-default-excludes
                     Send .git, node_modules, __pycache__ and friends too;
                     by default they are skipped
  --delete           Make the remote tree mirror the local one, deleting what
                     you deleted. Needs rsync on the box.
  --print            Print the command that would run, and exit

  run also takes:
  --dir <dir>        Directory to run in (default: the push destination)
  --no-push          Run without sending anything first
  --detach           Start it and return. The run keeps going after you
                     disconnect; read it back with "aq logs". Prints the run
                     id on stdout so you can capture it.

  A .aqignore file in the directory you send adds exclude patterns, one per
  line, "#" for comments.

  Transport: rsync when both ends have it (only changed files move), otherwise
  tar over ssh, which re-sends the whole tree. aq prints which one it used.

ls / logs:
  aq ls                      Live deployments: id, name, status, GPU, provider,
                              hourly rate and age
  aq ls --all                Include closed and failed ones

  The rate column always names its currency (e.g. "0.4200 USD"). It is the
  provider's own denomination, which is not always dollars.

  aq logs [name|id]          Print the most recent detached run's output
  aq logs --run <id>         Read one specific run
  aq logs --list             List this box's runs, their state, and command
  -f                         Keep streaming as the run writes more
  -n <lines>                 Trailing lines to show (default 200)
  --dir <dir>                Working directory the run was launched in
                              (default: /workspace)

idle:
  A PER-DEPLOYMENT idle-auto-pause policy (warn/pause thresholds, GPU idle %).
  It always outranks a setup's own "aq autopause" preference below, see
  "autopause" for how the two differ.

  aq idle status <name|id>   Show the deployment's idle-auto-pause policy and
                              its current live verdict (ACTIVE / IDLE / UNKNOWN)
  aq idle set <name|id>      Update the policy (only the flags you pass change)

  --warn-after <duration>   Warn after this much idle time, e.g. 30m, 1h
  --pause-after <duration>  Auto-pause after this much idle time, e.g. 1h
  --gpu-threshold <percent> GPU utilization below which the box counts idle
  --on / --off              Enable / disable idle auto-pause

endpoint:
  aq endpoint create <setup> <version>   Make a setup version callable.
                              Requires --max-instances and --spend-cap-cents:
                              an endpoint hands out a GPU budget, so
                              neither ever defaults to unbounded.
                              (--name <name>, default: the setup's own name)
                              --on <alias>  Pin it to a box you already
                              attached (aq attach <alias>) instead of
                              renting hardware — that box bills nothing, so
                              --spend-cap-cents is not required with --on.
  aq endpoint point <name> <version>
                              Repoint an endpoint at a different version in
                              its lineage (also how you roll back).
  aq endpoint rm <name>      Remove an endpoint.

call / calls:
  aq call <endpoint> [--input file]
                              Make a call against an endpoint and print its
                              call id. --input is a JSON file of the
                              declared params; with no --input, the call is
                              made with no inputs.
  aq calls <endpoint>        List an endpoint's recent calls: id, status,
                              phase, and reason. "unservable" means Aquanode
                              could not get the call a box at all, not that
                              the call's own code failed.

status / save / share / fork / edit-version / pause / autopause /
force-detach / sync-now / setups / down:
  aq status <name|id>        Re-check a provisioning or running setup
                             (add --show-secrets to print the password)
  aq save <name|id>          Save the setup's current state into its named
                             save lineage. The first save on a
                             setup asks for a lineage name once (Enter
                             accepts the default, which is the setup's own
                             name; a non-interactive shell just uses the
                             default). Every later save reuses that lineage
                             silently and increments its version (v1, v2,
                             v3, ...). (--name <lineage>, --path <dir>)
  aq share <name|id> <ver>   Print a link to ONE immutable saved version
                             (e.g. "aq share comfyui 3"). The link always
                             points at that exact version, never at
                             whatever the lineage's head becomes later.
  aq fork <token|link>       Turn a link from "aq share" (someone else's,
                             or your own team's own share of a team you've
                             since left) into a brand new setup in your own
                             library. Registers ownership only. It does
                             not itself boot any hardware.
                             (--name <name>, default: derived from the source)
  aq edit-version <name|id> <ver> [flags]
                             Edit a saved version's label, description,
                             and/or visibility. Only the flags you pass
                             change; there is currently no way to clear
                             a label/description back to empty.
                             (--label <text>, --description <text>,
                             --visibility private|team|public)
  aq pause <name|id>         Save the setup, then release its machine.
                             Pick it back up with "aq deploy --snapshot <id>"
                             (the paused deployment's id, which pause prints).
  aq autopause <name|id> on|off
                             Turn this SETUP's auto-pause-when-idle
                             preference on or off, using the platform's
                             default idle thresholds. This is NOT "aq idle"
                             above: idle policy is a per-DEPLOYMENT threshold
                             config that always outranks this, and this
                             carries no thresholds of its own: use "aq idle
                             set" to change WHEN idle counts as idle, and
                             this to turn auto-pause on setups on/off at all.
  aq force-detach <name|id> --yes
                             Break the setup's lease even mid-sync, for
                             when a deployment died holding it and it needs
                             freeing before anything else can attach.
                             --yes acknowledges work since the last
                             completed sync may be lost; there is no
                             silent form of this command.
  aq sync-now <name|id>      Force a sync tick right now instead of waiting
                             for the setup's own schedule, e.g. right
                             before "aq share"/"aq fork" so the link
                             reflects your latest work. Requires the setup
                             to be attached to a running deployment.
  aq setups                  List the setups you own: name, whether it's
                             running, latest saved version, and size.
  aq down <name|id>          Tear the setup down and stop billing
                             (--save saves first; terminate is skipped
                             if the save fails)

Environment:
  AQ_API_URL      Aquanode API base (default https://server.aquanode.io/api/v1)
  AQ_CONSOLE_URL  Aquanode console base "aq share" links point at
                  (default https://console.aquanode.io)
  AQ_CONFIG_DIR   Credential directory (default <user-config-dir>/aq)
  AQ_SSH_KEY      Private key to use for box access (default: your ~/.ssh key,
                  else aq's managed ~/.ssh/aquanode_ed25519)
  AQ_NO_BROWSER   Set to skip auto-opening the approval URL
  AQ_ALLOW_PROD   Set to 1 to allow "aq up"/"aq deploy"/"aq import" to rent
                  hardware on a non-local host from a script or other
                  non-interactive shell. Same effect as passing --prod.
                  Typing at a terminal needs neither.
`)
}
