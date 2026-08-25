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

// forkOptions configures runFork. fork() fills in the real environment;
// tests call runFork directly.
type forkOptions struct {
	cred  *config.Credential
	token string
	name  string
	out   io.Writer
}

// fork parses `aq fork <token|link> [--name <name>]` and wires the real
// environment into runFork.
//
// The consuming half of `aq share`'s link: turns a live share token into a
// brand new setup in the caller's own library. Forking your own team's own
// version is refused server-side.
func fork(args []string) error {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	name := fs.String("name", "", "name for the forked setup (default: derived from the source)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("a share token or link is required — usage: aq fork <token|link>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runFork(forkOptions{
		cred:  cred,
		token: extractShareToken(positional[0]),
		name:  *name,
		out:   os.Stdout,
	})
}

// extractShareToken accepts either a bare share token or a full share link
// (`aq share`'s <console>/launch/<token> output) and returns just the token,
// so pasting the whole link works without the user extracting it by hand.
func extractShareToken(raw string) string {
	raw = strings.TrimSpace(raw)
	const marker = "/launch/"
	if idx := strings.LastIndex(raw, marker); idx != -1 {
		return raw[idx+len(marker):]
	}
	return raw
}

// runFork turns a live share token into a new Setup the caller owns.
//
// Forking only registers ownership of the lineage — it does not itself boot
// any hardware, and aq has no verb yet to install/run a version onto a fresh
// box (that flow is console-only today: install-preview + install). So the
// success message points at `aq setups` to see the new entry rather than
// implying `aq up`/`aq deploy` can bring it online, which they can't.
func runFork(opts forkOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	res, err := client.ForkSetup(api.ForkSetupRequest{Token: opts.token, Name: opts.name})
	if err != nil {
		return fmt.Errorf("could not fork shared setup: %w", err)
	}

	fmt.Fprintf(out, "✓ Forked into %q — see it with `aq setups`. aq has no install/run-version verb yet; bring it online from the console for now.\n", res.Name)
	return nil
}
