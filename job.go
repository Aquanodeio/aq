package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// job dispatches `aq job <sub>` — the whole Jobs vocabulary, batch shape
// only (`aq endpoint` is the service-shape sibling, untouched by the
// jobs-are-jobs spec).
//
// A job is 1:1 with its one Run now: `create` (make it, do nothing), `point`
// (repoint a running job at a different version) and `runs` (list more than
// one run) are GONE, not renamed — there is nothing left for any of them to
// mean. `run` both creates the job and starts it; running it again makes a
// NEW job via `rerun`.
//
// A GROUP rather than top-level verbs: `aq run` and `aq logs` already mean
// "push this directory to a box and run something on it" and "tail a box's
// logs". Those are daily commands, and `aq run mybox` / `aq run myjob` are
// the same string, so there is no argument shape that could disambiguate
// them.
func job(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq job <run|ls|logs|cancel|pull|rerun|rm> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "run":
		return jobRun(rest)
	case "ls":
		return jobLs(rest)
	case "logs":
		return jobLogs(rest)
	case "cancel":
		return jobCancel(rest)
	case "pull":
		return jobPull(rest)
	case "rerun":
		return jobRerun(rest)
	case "rm":
		return jobRemove(rest)
	default:
		return fmt.Errorf("aq job: unknown subcommand %q, expected one of run, ls, logs, cancel, pull, rerun, rm", sub)
	}
}

// jobRemoveOptions configures runJobRemove. jobRemove() fills
// in the real environment; tests run runJobRemove directly.
type jobRemoveOptions struct {
	cred   *config.Credential
	target string // job id or name
	out    io.Writer
}

// jobRemove parses `aq job rm <name>` and wires the real
// environment into runJobRemove.
func jobRemove(args []string) error {
	fs := flag.NewFlagSet("job rm", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq job rm <name>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobRemove(jobRemoveOptions{cred: cred, target: target, out: os.Stdout})
}

// runJobRemove deletes a job.
func runJobRemove(opts jobRemoveOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}
	ep, err := findJob(client, jobID)
	if err != nil {
		return err
	}

	if err := client.DeleteJob(jobID); err != nil {
		return fmt.Errorf("could not remove job %q: %w", ep.Name, err)
	}

	fmt.Fprintf(out, "✓ Removed job %q\n", ep.Name)
	return nil
}
