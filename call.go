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

// callOptions configures runCall. call() fills in the real environment;
// tests call runCall directly.
type callOptions struct {
	cred        *config.Credential
	target      string // endpoint id or name
	inputs      map[string]any
	wait        bool
	waitSeconds int
	out         io.Writer
}

// call parses `aq call <endpoint> [--input file] [--wait [--wait-seconds <n>]]`
// and wires the real environment into runCall.
func call(args []string) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	inputPath := fs.String("input", "", "path to a JSON file of the declared params (default: no inputs)")
	wait := fs.Bool("wait", false, "wait for the call to complete (up to --wait-seconds, default 30)")
	waitSeconds := fs.Int("wait-seconds", 30, "maximum seconds to wait for completion (only meaningful with --wait, capped at 120)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq call <endpoint> [--input file] [--wait [--wait-seconds <n>]]")
	}
	target := positional[0]

	inputs, err := loadCallInputs(*inputPath)
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runCall(callOptions{
		cred:        cred,
		target:      target,
		inputs:      inputs,
		wait:        *wait,
		waitSeconds: *waitSeconds,
		out:         os.Stdout,
	})
}

// loadCallInputs reads --input's JSON file into the map the API sends as
// `inputs`, or returns an empty (non-nil) map when no --input was given —
// the wire body is always `{"inputs":{}}` at minimum, never an omitted or
// nil field.
func loadCallInputs(path string) (map[string]any, error) {
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

// runCall makes a call against an endpoint and prints the result. When --wait
// is used, it may return the completed call object (200) or still-running status
// (202). Either way, exit 0 — a timeout waiting for completion is not an error.
func runCall(opts callOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	endpointID, err := resolveEndpointID(client, opts.target)
	if err != nil {
		return err
	}

	req := api.CreateCallRequest{Inputs: opts.inputs}
	if opts.wait {
		req.Wait = true
		req.WaitSeconds = opts.waitSeconds
	}

	res, err := client.CreateCall(endpointID, req)
	if err != nil {
		return fmt.Errorf("could not call endpoint %q: %w", opts.target, err)
	}

	// If --wait was used and the call completed (200 response), res.ID will be
	// set and res.CallID will be empty. Otherwise, it's the usual 202 async
	// response with CallID set.
	if opts.wait && !res.IsAsync() {
		// 200 response: call completed within the wait window
		fmt.Fprintf(out, "✓ Call %s %s\n", res.ID, res.Status)
		if res.Status == "unservable" {
			// unservable means Aquanode could not get a box; the reason is important
			fmt.Fprintf(out, "  (reason: %s)\n", res.Reason)
		}
		if res.OutputRef != "" {
			fmt.Fprintf(out, "  output: %s\n", res.OutputRef)
		}
		return nil
	}

	// 202 response: call is still running or no warm box available
	if opts.wait {
		// User asked to wait but it didn't complete in time
		fmt.Fprintf(out, "Call %s is still running.\n", res.GetCallID())
		fmt.Fprintf(out, "Check its status with: aq calls %s\n", opts.target)
		return nil
	}

	// No --wait: standard async response
	fmt.Fprintf(out, "✓ Call %s %s\n", res.GetCallID(), res.Status)
	return nil
}
