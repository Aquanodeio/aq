// Command aq is the Aquanode control / funnel CLI.
//
// It runs on a developer's laptop and talks to the Aquanode API to rent GPUs,
// provision the ogre on-box agent, and restore snapshots. It is a thin
// orchestration wrapper over `ogre` (the OSS on-box agent) + the Aquanode API —
// it does not reimplement ogre.
//
// This is a scaffold: subcommands (login / deploy / up) are built out by the
// funnel tickets. See research/action/02-console-dx.md in the meta-repo.
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

	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Printf("aq %s\n", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "aq: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `aq — Aquanode control CLI

Usage:
  aq <command> [flags]

Commands:
  version    Print the aq version
  help       Show this help

  (login / deploy / up are added by the funnel tickets)
`)
}
