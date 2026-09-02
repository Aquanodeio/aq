package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// `aq job logs <job> <run-id> [-f]` — tail one run's log.
//
// Byte-offset paging, advancing by the offset the SERVER returns rather than by
// the length of what we printed. A capped read would otherwise make the
// follower skip bytes it never saw, and a log with a silent hole in it is worse
// than a slow one.

type jobLogsOptions struct {
	cred    *config.Credential
	jobRef  string
	runID   string
	follow  bool
	attempt int
	out     io.Writer
	errOut  io.Writer

	client   func(cred *config.Credential) *api.Client
	sleep    func(time.Duration)
	maxPolls int // 0 = unlimited; tests bound it
}

func jobLogs(args []string) error {
	fs := flag.NewFlagSet("job logs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	follow := fs.Bool("f", false, "keep printing as the run writes more")
	followLong := fs.Bool("follow", false, "keep printing as the run writes more")
	attempt := fs.Int("attempt", 0, "which attempt's log (default: the latest)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("aq job logs: %w", err)
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return errors.New("usage: aq job logs <job> <run-id> [-f] [--attempt N]")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return runJobLogs(jobLogsOptions{
		cred:    cred,
		jobRef:  rest[0],
		runID:   rest[1],
		follow:  *follow || *followLong,
		attempt: *attempt,
		out:     os.Stdout,
		errOut:  os.Stderr,
	})
}

func runJobLogs(opts jobLogsOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.errOut
	if errOut == nil {
		errOut = os.Stderr
	}
	newClient := opts.client
	if newClient == nil {
		newClient = newControlClient
	}
	sleep := opts.sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	client := newClient(opts.cred)
	jobID, err := resolveJobID(client, opts.jobRef)
	if err != nil {
		return err
	}

	var offset int64
	var warnedUnreachable bool
	for polls := 0; ; polls++ {
		chunk, err := client.GetRunLogs(jobID, opts.runID, offset, opts.attempt)
		if err != nil {
			return fmt.Errorf("aq job logs: %w", err)
		}

		if chunk.Chunk != "" {
			fmt.Fprint(out, chunk.Chunk)
			offset = chunk.NextOffset
		}

		// Said ONCE, to stderr, and never as an empty log. "We could not read
		// it" and "there is nothing to read" are different facts, and printing
		// nothing would silently assert the second.
		if chunk.Source == "unreachable" && !warnedUnreachable {
			fmt.Fprintln(errOut, "aq: can't reach the machine to read its log right now — this says nothing about whether your run is still going")
			warnedUnreachable = true
		}
		if chunk.Truncated {
			fmt.Fprintln(errOut, "aq: this log got long enough that its oldest output was dropped; you are seeing the retained tail")
		}

		if !opts.follow {
			return nil
		}
		if chunk.Source == "archived" || chunk.Source == "box_gone" {
			// Nothing further will ever be written. Following forever here
			// would look like a hang.
			return nil
		}
		if opts.maxPolls > 0 && polls+1 >= opts.maxPolls {
			return nil
		}
		sleep(2 * time.Second)
	}
}

// `aq job cancel <job> <run-id>`.
func jobCancel(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: aq job cancel <job> <run-id>")
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	client := newControlClient(cred)
	jobID, err := resolveJobID(client, args[0])
	if err != nil {
		return err
	}
	run, err := client.CancelRun(jobID, args[1])
	if err != nil {
		return fmt.Errorf("aq job cancel: %w", err)
	}
	fmt.Fprintf(os.Stdout, "cancelling %s — billing stops when the machine is released\n", run.ID)
	return nil
}
