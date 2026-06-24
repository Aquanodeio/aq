package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// downOptions configures runDown. down() fills in the real environment; tests
// inject a base URL and a buffer writer.
type downOptions struct {
	cred   *config.Credential
	target string // numeric deployment id or a project id (resolved by runDown)
	out    io.Writer
}

// down parses the deployment target and wires the real environment into runDown.
//
// `aq down <deploymentId>` tears down an env brought up by `aq up` / `aq deploy`,
// stopping the rented GPU box and its billing.
func down(args []string) error {
	target, err := parseDeploymentTarget(args, "down")
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runDown(downOptions{
		cred:   cred,
		target: target,
		out:    os.Stdout,
	})
}

// runDown requests termination of the deployment and reports the outcome.
func runDown(opts downOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}

	client := newControlClient(opts.cred)

	deploymentID, err := resolveDeploymentID(client, opts.target, "down")
	if err != nil {
		return err
	}

	if _, err := client.CloseDeployment(deploymentID); err != nil {
		return fmt.Errorf("could not close deployment #%d: %w", deploymentID, err)
	}

	fmt.Fprintf(opts.out, "✓ Termination requested for deployment #%d — the box will stop shortly.\n", deploymentID)
	return nil
}

// newControlClient builds an authenticated API client from a stored credential,
// defaulting the base URL to the configured Aquanode API.
func newControlClient(cred *config.Credential) *api.Client {
	apiURL := cred.APIURL
	if apiURL == "" {
		apiURL = config.APIURL()
	}
	return api.NewAuthed(apiURL, cred.Token, cred.TeamID)
}

// parseDeploymentTarget reads a single positional `aq <verb> <id>` token. It is
// either the numeric deployment id shown by `aq up`/`aq deploy`/the console, or
// a project id — resolveDeploymentID tells the two apart.
func parseDeploymentTarget(args []string, verb string) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return "", fmt.Errorf("a deployment id is required — usage: aq %s <deploymentId>", verb)
	}
	return args[0], nil
}

// resolveDeploymentID turns a positional token into a numeric deployment id. A
// numeric token IS the deployment id; a non-numeric token is treated as a
// project id (the UUID in the console URL) and resolved to its current
// deployment via the API — so a user who pastes a project id gets the box they
// meant instead of a cryptic "expected a number" error, and an unknown token
// gets a message pointing at the numeric deployment id (#209).
func resolveDeploymentID(client *api.Client, target, verb string) (int, error) {
	if id, err := strconv.Atoi(target); err == nil {
		if id <= 0 {
			return 0, fmt.Errorf("invalid deployment id %q — expected a positive number", target)
		}
		return id, nil
	}

	dep, err := client.GetProjectDeployment(target)
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return 0, fmt.Errorf("no active deployment found for %q — pass the numeric deployment id (e.g. aq %s 4242) shown by `aq up`/`aq deploy` or the console", target, verb)
		}
		return 0, fmt.Errorf("could not resolve %q to a deployment: %w", target, err)
	}
	if dep.ID <= 0 {
		return 0, fmt.Errorf("no active deployment found for project %q — pass the numeric deployment id (e.g. aq %s 4242)", target, verb)
	}
	return dep.ID, nil
}
