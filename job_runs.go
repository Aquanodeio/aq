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

// jobRunsOptions configures runRuns. runs() fills in the real environment;
// tests run runRuns directly.
type jobRunsOptions struct {
	cred   *config.Credential
	target string // job id or name
	out    io.Writer
}

// runs parses `aq runs <job>` and wires the real environment into
// runRuns.
func jobRuns(args []string) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq runs <job>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return doJobRuns(jobRunsOptions{cred: cred, target: target, out: os.Stdout})
}

// runRuns fetches and renders a job's recent runs.
func doJobRuns(opts jobRunsOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}

	list, err := client.ListRuns(jobID)
	if err != nil {
		return fmt.Errorf("could not list runs for job %q: %w", opts.target, err)
	}

	printRuns(out, list)
	return nil
}

// printRuns renders the run list as a simple aligned table, or a one-line
// nudge when the job has never been called.
//
// REASON is always its own column, printed for every row — not just the
// failed/unservable ones. "unservable" must read as visibly distinct from
// "failed": it means Aquanode could not get the caller a box at all
// (capacity, budget, or provider failure, before the workload ever ran),
// while "failed" means the workload ran and errored. Collapsing that
// distinction into one status word would hide exactly the case where it
// matters most — whose fault the failure was.
func printRuns(out io.Writer, list []api.Run) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No runs yet. Run `aq run <job>` to make one.")
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
