package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// jobRerunOptions configures runJobRerun. jobRerun() fills in the real
// environment; tests run runJobRerun directly.
type jobRerunOptions struct {
	cred   *config.Credential
	target string // source job id or name
	out    io.Writer
}

// jobRerun parses `aq job rerun <job>` and wires the real environment into
// runJobRerun.
//
// "Running it again" always makes a NEW job (the jobs-are-jobs contract):
// POST /jobs/:id/rerun copies
// the source job's spec into a fresh job and dispatches its own new Run in
// the same call, same as `aq job run` does for a brand-new one. The source
// job named here is never mutated or re-run in place.
func jobRerun(args []string) error {
	fs := flag.NewFlagSet("job rerun", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq job rerun <job>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobRerun(jobRerunOptions{cred: cred, target: positional[0], out: os.Stdout})
}

// runJobRerun posts POST /jobs/:id/rerun and prints the newly created job's
// id, name and run, and where to watch it — never streams its log itself,
// unlike `aq job run`'s default: the source job is very likely one someone
// is inspecting from `aq job ls` after it already finished, not a
// fire-and-watch just typed, so the safer default is to hand back the new
// job's address and let the caller ask for its log with `aq job logs`.
func runJobRerun(opts jobRerunOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}

	job, err := client.RerunJob(jobID)
	if err != nil {
		return fmt.Errorf("could not rerun job %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Rerun %q → new job %q, run %s %s\n", opts.target, job.Name, job.Run.ID, jobStatusWord(job.Run.Status))
	fmt.Fprintf(out, "  aq job logs %s\n", job.Name)
	return nil
}
