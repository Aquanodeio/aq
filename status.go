package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/config"
)

// statusOptions configures runStatus. status() fills in the real environment;
// tests inject a base URL and a buffer writer.
type statusOptions struct {
	cred   *config.Credential
	target string // numeric deployment id or a project id (resolved by runStatus)
	out    io.Writer
}

// status parses the deployment target and wires the real environment into runStatus.
//
// `aq status <deploymentId>` re-checks a deployment started by `aq up` — useful
// when `aq up` hits its provisioning timeout and tells the user to come back.
func status(args []string) error {
	target, err := parseDeploymentTarget(args, "status")
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runStatus(statusOptions{
		cred:   cred,
		target: target,
		out:    os.Stdout,
	})
}

// runStatus fetches the deployment's status and prints it, plus the live HTTPS
// URL + service credentials once ogre has published them.
func runStatus(opts statusOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}

	client := newControlClient(opts.cred)

	deploymentID, err := resolveDeploymentID(client, opts.target, "status")
	if err != nil {
		return err
	}

	res, err := client.DeploymentStatus(deploymentID)
	if err != nil {
		return fmt.Errorf("could not fetch status for deployment #%d: %w", deploymentID, err)
	}

	state := res.Deployment.Status
	if state == "" {
		state = res.Status
	}
	if state == "" {
		state = "UNKNOWN"
	}
	fmt.Fprintf(opts.out, "Deployment #%d: %s\n", deploymentID, state)

	creds := res.Deployment.ServiceCredentials
	if creds != nil && creds.URL != "" {
		fmt.Fprintf(opts.out, "\n%s is live:\n\n    %s\n\n", templateLabel(creds.Template), creds.URL)
		if creds.Username != "" {
			fmt.Fprintf(opts.out, "  Username: %s\n", creds.Username)
		}
		if creds.Password != "" {
			fmt.Fprintf(opts.out, "  Password: %s\n", creds.Password)
		}
		return nil
	}

	if isClosedStatus(state) {
		fmt.Fprintf(opts.out, "\nThis deployment is no longer running.\n")
		return nil
	}

	fmt.Fprintf(opts.out, "\nStill provisioning — re-run `aq status %d` in a minute.\n", deploymentID)
	return nil
}

// requireLogin loads the stored credential, erroring if the CLI is not paired.
func requireLogin() (*config.Credential, error) {
	cred, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cred == nil || cred.Token == "" {
		return nil, errors.New("not logged in — run `aq login` first")
	}
	return cred, nil
}
