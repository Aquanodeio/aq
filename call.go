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
	cred   *config.Credential
	target string // endpoint id or name
	inputs map[string]any
	out    io.Writer
}

// call parses `aq call <endpoint> [--input file]` and wires the real
// environment into runCall.
func call(args []string) error {
	fs := flag.NewFlagSet("call", flag.ContinueOnError)
	inputPath := fs.String("input", "", "path to a JSON file of the declared params (default: no inputs)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq call <endpoint> [--input file]")
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

	return runCall(callOptions{cred: cred, target: target, inputs: inputs, out: os.Stdout})
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

// runCall makes a call against an endpoint and prints the call id — the
// handle needed to poll it back via `aq calls`.
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

	res, err := client.CreateCall(endpointID, api.CreateCallRequest{Inputs: opts.inputs})
	if err != nil {
		return fmt.Errorf("could not call endpoint %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Call %s %s\n", res.CallID, res.Status)
	return nil
}
