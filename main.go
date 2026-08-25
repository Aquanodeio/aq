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
	switch cmd {
	case "version", "--version", "-v":
		fmt.Printf("aq %s\n", version)
	case "help", "--help", "-h":
		usage()
	case "login":
		run(login(args))
	case "up":
		run(up(args))
	case "deploy":
		run(deploy(args))
	case "ssh":
		run(sshCmd(args))
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
	case "park":
		run(park(args))
	case "autosave":
		run(autosave(args))
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
	fmt.Fprint(os.Stderr, `aq — Aquanode control CLI

Usage:
  aq <command> [flags]

Commands:
  login         Pair this CLI to your Aquanode account (device login)
  up            Rent the cheapest matching GPU and bring up a working setup
  deploy        Restore a save onto a freshly-rented Aquanode GPU box
  ssh           Open a shell on a setup (managed key + ~/.ssh/config alias)
  status        Show a setup's status, HTTPS URL, and credentials
  save          Save a setup's current state into its named lineage
  share         Get a link to one saved version of a setup
  fork          Turn a share link into a new setup in your own library
  edit-version  Edit a saved version's label, description, or visibility
  park          Save a setup, then release its machine (resume later with up)
  autosave      Turn a setup's automated snapshotting on or off
  autopause     Turn a setup's stop-when-idle preference on or off
  force-detach  Break a setup's lease even mid-sync (can lose unsynced work)
  sync-now      Force a setup's sync tick right now
  setups        List the setups you own
  idle          View or change a DEPLOYMENT's idle-auto-stop thresholds
  endpoint      Make a setup version callable, repoint it, or remove it
  call          Make a call against an endpoint
  calls         List an endpoint's recent calls
  down          Tear down a setup (stop the rented GPU box)
  logout        Remove the stored CLI credential
  whoami        Show the current login state
  version       Print the aq version
  help          Show this help

up flags:
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --max-price <n>    Only rent GPUs at or below this hourly price
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)
  --auto-stop        Enable idle auto-stop on this deployment (off by default)
  --warn-after <duration>  With --auto-stop: warn after this much idle time
  --stop-after <duration>  With --auto-stop: auto-stop after this much idle time

  App (optional — ComfyUI installs by default if you pick neither):
  --comfyui          Install ComfyUI
  --jupyter          Install Torch + Jupyter instead

deploy flags:
  --snapshot <id>    Save to deploy (id from aq / the console, e.g. ext-42)
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --max-price <n>    Only rent GPUs at or below this hourly price
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)

  App (optional — relaunches ComfyUI by default; --no-app restores data only):
  --comfyui          Relaunch ComfyUI on the restored data
  --jupyter          Relaunch Torch + Jupyter on the restored data instead
  --no-app           Restore only — do not relaunch an app

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

idle:
  A PER-DEPLOYMENT idle-auto-stop policy (warn/stop thresholds, GPU idle %).
  It always outranks a setup's own "aq autopause" preference below — see
  "autopause" for how the two differ.

  aq idle status <name|id>   Show the deployment's idle-auto-stop policy and
                              its current live verdict (ACTIVE / IDLE / UNKNOWN)
  aq idle set <name|id>      Update the policy (only the flags you pass change)

  --warn-after <duration>   Warn after this much idle time, e.g. 30m, 1h
  --stop-after <duration>   Auto-stop after this much idle time, e.g. 1h
  --gpu-threshold <percent> GPU utilization below which the box counts idle
  --on / --off              Enable / disable idle auto-stop

endpoint:
  aq endpoint create <setup> <version>   Make a setup version callable.
                              Requires --max-instances and --spend-cap-cents
                              — an endpoint hands out a GPU budget, so
                              neither ever defaults to unbounded.
                              (--name <name>, default: the setup's own name)
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
                              could not get the call a box at all — not that
                              the call's own code failed.

status / save / share / fork / edit-version / park / autosave / autopause /
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
                             library. Registers ownership only — it does
                             not itself boot any hardware.
                             (--name <name>, default: derived from the source)
  aq edit-version <name|id> <ver> [flags]
                             Edit a saved version's label, description,
                             and/or visibility. Only the flags you pass
                             change; there is currently no way to clear
                             a label/description back to empty.
                             (--label <text>, --description <text>,
                             --visibility private|team|public)
  aq park <name|id>          Save the setup, then release its machine.
                             Pick it back up any time with "aq up". Named
                             "park", not "pause" — console's own "pause"
                             already names a different thing (pausing the
                             automated-snapshot cron on the older Snapshotter
                             tab), so this avoids the collision.
  aq autosave <name|id> on|off
                             Turn automated snapshotting on or off. This
                             keeps ONE always-current copy — it is NOT a
                             history and NOT undo: deleting your own work
                             is replicated into that copy on the next
                             tick too. Held snapshot storage is billed
                             at `+heldStorageRateLabel+`; turning it on prints
                             that rate.
  aq autopause <name|id> on|off
                             Turn this SETUP's stop-when-idle preference on
                             or off, using the platform's default idle
                             thresholds. This is NOT "aq idle" above: idle
                             policy is a per-DEPLOYMENT threshold config
                             that always outranks this, and this carries no
                             thresholds of its own — use "aq idle set" to
                             change WHEN idle counts as idle, and this to
                             turn stopping on setups on/off at all.
  aq force-detach <name|id> --yes
                             Break the setup's lease even mid-sync — for
                             when a deployment died holding it and it needs
                             freeing before anything else can attach.
                             --yes acknowledges work since the last
                             completed sync may be lost; there is no
                             silent form of this command.
  aq sync-now <name|id>      Force a sync tick right now instead of waiting
                             for the setup's own schedule — e.g. right
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
`)
}
