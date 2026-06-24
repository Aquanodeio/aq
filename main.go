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
  logout     Remove the stored CLI credential
  whoami     Show the current login state
  version    Print the aq version
  help       Show this help

  (deploy / up are added by later funnel tickets)

Environment:
  AQ_API_URL      Aquanode API base (default https://server.aquanode.io/api/v1)
  AQ_CONFIG_DIR   Credential directory (default <user-config-dir>/aq)
  AQ_NO_BROWSER   Set to skip auto-opening the approval URL
`)
}
