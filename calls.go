package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// callsOptions configures runCalls. calls() fills in the real environment;
// tests call runCalls directly.
type callsOptions struct {
	cred   *config.Credential
	target string // endpoint id or name
	out    io.Writer
}

// calls parses `aq calls <endpoint>` and wires the real environment into
// runCalls.
func calls(args []string) error {
	fs := flag.NewFlagSet("calls", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq calls <endpoint>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runCalls(callsOptions{cred: cred, target: target, out: os.Stdout})
}

// runCalls fetches and renders an endpoint's recent calls.
func runCalls(opts callsOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	endpointID, err := resolveEndpointID(client, opts.target)
	if err != nil {
		return err
	}

	list, err := client.ListCalls(endpointID)
	if err != nil {
		return fmt.Errorf("could not list calls for endpoint %q: %w", opts.target, err)
	}

	printCalls(out, list)
	return nil
}

// printCalls renders the call list as a simple aligned table, or a one-line
// nudge when the endpoint has never been called.
//
// REASON is always its own column, printed for every row — not just the
// failed/unservable ones. "unservable" must read as visibly distinct from
// "failed": it means Aquanode could not get the caller a box at all
// (capacity, budget, or provider failure, before the workload ever ran),
// while "failed" means the workload ran and errored. Collapsing that
// distinction into one status word would hide exactly the case where it
// matters most — whose fault the failure was.
func printCalls(out io.Writer, list []api.Call) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No calls yet — run `aq call <endpoint>` to make one.")
		return
	}

	fmt.Fprintf(out, "%-36s  %-11s  %-9s  %s\n", "ID", "STATUS", "PHASE", "REASON")
	for _, c := range list {
		status := c.Status
		if status == "unservable" {
			status = "UNSERVABLE"
		}
		fmt.Fprintf(out, "%-36s  %-11s  %-9s  %s\n", c.ID, status, c.Phase, c.Reason)
	}
}
