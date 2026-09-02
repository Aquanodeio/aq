package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// jobRunOptions configures runRun. run() fills in the real environment;
// tests run runRun directly.
type jobRunOptions struct {
	cred        *config.Credential
	target      string // job id or name
	inputs      map[string]any
	wait        bool
	waitSeconds int
	out         io.Writer
}

// run parses `aq run <job> [--input file] [--wait [--wait-seconds <n>]]`
// and wires the real environment into runRun.
func jobRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	inputPath := fs.String("input", "", "path to a JSON file of the declared params (default: no inputs)")
	wait := fs.Bool("wait", false, "wait for the run to complete (up to --wait-seconds, default 30)")
	waitSeconds := fs.Int("wait-seconds", 30, "maximum seconds to wait for completion (only meaningful with --wait, capped at 120)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq run <job> [--input file] [--wait [--wait-seconds <n>]]")
	}
	target := positional[0]

	inputs, err := loadJobRunInputs(*inputPath)
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return doJobRun(jobRunOptions{
		cred:        cred,
		target:      target,
		inputs:      inputs,
		wait:        *wait,
		waitSeconds: *waitSeconds,
		out:         os.Stdout,
	})
}

// loadJobRunInputs reads --input's JSON file into the map the API sends as
// `inputs`, or returns an empty (non-nil) map when no --input was given —
// the wire body is always `{"inputs":{}}` at minimum, never an omitted or
// nil field.
func loadJobRunInputs(path string) (map[string]any, error) {
	if path == "" {
		return map[string]any{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read --input file %q: %w", path, err)
	}
	var inputs map[string]any
	if err := json.Unmarshal(data, &inputs); err != nil {
		return nil, fmt.Errorf("could not parse --input file %q as JSON: %w", path, err)
	}
	if inputs == nil {
		inputs = map[string]any{}
	}
	return inputs, nil
}

// runRun makes a run against a job and prints the result. When --wait
// is used, it may return the completed run object (200) or still-running status
// (202). Either way, exit 0 — a timeout waiting for completion is not an error.
func doJobRun(opts jobRunOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}

	req := api.CreateRunRequest{Inputs: opts.inputs}
	if opts.wait {
		req.Wait = true
		req.WaitSeconds = opts.waitSeconds
	}

	res, err := client.CreateRun(jobID, req)
	if err != nil {
		return fmt.Errorf("could not run job %q: %w", opts.target, err)
	}

	// If --wait was used and the run completed (200 response), res.ID will be
	// set and res.RunID will be empty. Otherwise, it's the usual 202 async
	// response with RunID set.
	if opts.wait && !res.IsAsync() {
		// 200 response: run completed within the wait window
		fmt.Fprintf(out, "✓ Run %s %s\n", res.ID, res.Status)
		if res.Status == "unservable" {
			// unservable means Aquanode could not get a box; the reason is important
			fmt.Fprintf(out, "  (reason: %s)\n", res.Reason)
		}
		if res.OutputRef != "" {
			fmt.Fprintf(out, "  output: %s\n", res.OutputRef)
		}
		return nil
	}

	// 202 response: run is still running or no warm box available
	if opts.wait {
		// User asked to wait but it didn't complete in time
		fmt.Fprintf(out, "Run %s is still running.\n", res.GetRunID())
		fmt.Fprintf(out, "Check its status with: aq runs %s\n", opts.target)
		return nil
	}

	// No --wait: standard async response
	fmt.Fprintf(out, "✓ Run %s %s\n", res.GetRunID(), res.Status)
	return nil
}
