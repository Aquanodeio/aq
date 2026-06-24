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
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
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
	case "status":
		run(status(args))
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
  login      Pair this CLI to your Aquanode account (device login)
  up         Rent the cheapest matching GPU and bring up a working env
  deploy     Restore a snapshot onto a freshly-rented Aquanode GPU box
  status     Show a deployment's status, HTTPS URL, and credentials
  down       Tear down a deployment (stop the rented GPU box)
  logout     Remove the stored CLI credential
  whoami     Show the current login state
  version    Print the aq version
  help       Show this help

up flags:
  --comfyui          Bring up ComfyUI (default)
  --jupyter          Bring up Torch + Jupyter
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --max-price <n>    Only rent GPUs at or below this hourly price
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)

deploy flags:
  --snapshot <id>    Snapshot to deploy (id from aq / the console, e.g. ext-42)
  --comfyui          Relaunch ComfyUI on the restored data (default)
  --jupyter          Relaunch Torch + Jupyter on the restored data
  --no-app           Restore only — do not relaunch an app
  --gpu <model>      Filter to a GPU model (substring, e.g. "RTX 4090")
  --max-price <n>    Only rent GPUs at or below this hourly price
  --provider <name>  Restrict to a single provider (e.g. massecompute)
  --show-secrets     Echo the service password to stdout (hidden by default)

status / down:
  aq status <deploymentId>   Re-check a provisioning or running env
                             (add --show-secrets to print the password)
  aq down <deploymentId>     Tear the env down and stop billing

Environment:
  AQ_API_URL      Aquanode API base (default https://server.aquanode.io/api/v1)
  AQ_CONFIG_DIR   Credential directory (default <user-config-dir>/aq)
  AQ_NO_BROWSER   Set to skip auto-opening the approval URL
`)
}
