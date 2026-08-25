package main

import (
	"fmt"
	"io"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveSSHAlias turns a user-supplied deployment target into the managed
// ssh_config alias that addresses it, refreshing that config on the way.
//
// Shared by `aq ssh`, `aq push`, and `aq run` so all three reject a dead or
// still-provisioning box with the same wording, and so all three agree that the
// thing you connect to is the alias — never a raw root@ip. verb names the
// caller for resolveDeploymentID's error text.
func resolveSSHAlias(client *api.Client, target, verb string, errOut io.Writer) (string, error) {
	deploymentID, err := resolveDeploymentID(client, target, verb)
	if err != nil {
		return "", err
	}

	res, err := client.DeploymentStatus(deploymentID)
	if err != nil {
		return "", fmt.Errorf("could not fetch status for deployment #%d: %w", deploymentID, err)
	}
	dep := res.Deployment
	if dep.ID == 0 {
		dep.ID = deploymentID
	}
	state := dep.Status
	if state == "" {
		state = res.Status
	}

	switch {
	case isClosedStatus(state):
		return "", fmt.Errorf("deployment #%d is %s — it is no longer running", deploymentID, state)
	case !isActiveStatus(state):
		return "", fmt.Errorf("deployment #%d is still provisioning (%s) — retry in a minute", deploymentID, state)
	}

	if _, _, ok := sshEndpointFor(dep); !ok {
		// Distinct from the provisioning message on purpose: a box whose provider
		// maps no port 22 (an Akash lease without an SSH mapping) will never grow
		// one, so "retry in a minute" would be a lie.
		if dep.AppURL == "" && len(dep.ServiceURLs) == 0 {
			return "", fmt.Errorf("deployment #%d is up but has not reported an address yet — retry in a moment", deploymentID)
		}
		return "", fmt.Errorf("deployment #%d does not expose SSH", deploymentID)
	}

	warnIfKeyUnregistered(client, errOut)

	// The alias must exist before we exec, and it is what we exec against — so
	// unlike the up/status paths this sync is fatal.
	if err := syncManagedConfig(client, []api.Deployment{dep}, 0); err != nil {
		return "", err
	}

	return aliasFor(dep.Name, dep.ID), nil
}
