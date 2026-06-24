package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// downOptions configures runDown. down() fills in the real environment; tests
// inject a base URL and a buffer writer.
type downOptions struct {
	cred         *config.Credential
	deploymentID int
	out          io.Writer
}

// down parses the deployment id and wires the real environment into runDown.
//
// `aq down <deploymentId>` tears down an env brought up by `aq up` / `aq deploy`,
// stopping the rented GPU box and its billing.
func down(args []string) error {
	deploymentID, err := parseDeploymentID(args, "down")
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runDown(downOptions{
		cred:         cred,
		deploymentID: deploymentID,
		out:          os.Stdout,
	})
}

// runDown requests termination of the deployment and reports the outcome.
func runDown(opts downOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}

	apiURL := opts.cred.APIURL
	if apiURL == "" {
		apiURL = config.APIURL()
	}
	client := api.NewAuthed(apiURL, opts.cred.Token, opts.cred.TeamID)

	if _, err := client.CloseDeployment(opts.deploymentID); err != nil {
		return fmt.Errorf("could not close deployment #%d: %w", opts.deploymentID, err)
	}

	fmt.Fprintf(opts.out, "✓ Termination requested for deployment #%d — the box will stop shortly.\n", opts.deploymentID)
	return nil
}

// parseDeploymentID reads a single positional deployment id from args. The id is
// the numeric deployment number shown by `aq up` / `aq deploy` / the console.
func parseDeploymentID(args []string, verb string) (int, error) {
	if len(args) == 0 || args[0] == "" {
		return 0, fmt.Errorf("a deployment id is required — usage: aq %s <deploymentId>", verb)
	}
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return 0, fmt.Errorf("invalid deployment id %q — expected a number (e.g. aq %s 4242)", args[0], verb)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid deployment id %q — expected a positive number", args[0])
	}
	return id, nil
}
