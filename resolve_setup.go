package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveSetupID resolves a user-typed `<setup>` argument to the setup's own
// id — a UUID (`model Setup { id String @id @default(uuid()) ... }`), NEVER
// a deployment id. A setup and the deployment currently renting its compute
// are different objects with different id spaces; conflating them addresses
// the wrong row.
//
// Resolution order: a target that already looks like a UUID is used as-is;
// otherwise it's matched against GET /setups by name, which is unique per
// team (`@@unique([teamId, name])`) so at most one match is ever expected.
// No match, or more than one (which would mean that uniqueness assumption
// broke), is a hard error naming what was tried — this never falls back to
// treating the input as, or resolving it via, a deployment id.
func resolveSetupID(client *api.Client, target string) (string, error) {
	if target == "" {
		return "", errors.New("a setup is required")
	}
	if looksLikeUUID(target) {
		return target, nil
	}

	setups, err := client.ListSetups()
	if err != nil {
		return "", fmt.Errorf("could not list setups: %w", err)
	}

	var matches []api.Setup
	for _, s := range setups {
		if strings.EqualFold(strings.TrimSpace(s.Name), target) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no setup named %q", target)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d setups — pass the setup id instead", target, len(matches))
	}
}

// findSetup fetches the caller's setups and returns the one matching id, so
// callers that already have a resolved id (from resolveSetupID or a direct
// UUID) can get at its other fields (LeaseDeploymentID, ...) without a
// dedicated GET /setups/:id endpoint.
func findSetup(client *api.Client, setupID string) (*api.Setup, error) {
	setups, err := client.ListSetups()
	if err != nil {
		return nil, fmt.Errorf("could not list setups: %w", err)
	}
	for i := range setups {
		if setups[i].ID == setupID {
			return &setups[i], nil
		}
	}
	return nil, fmt.Errorf("setup %q not found", setupID)
}

// setupIDForDeployment maps a deployment id to the setup whose lease it
// currently holds. `aq down --save` uses this: the checkpoint save it
// takes before terminating is a setup-scoped call, but the deployment being
// torn down is the only identifier the user gave it.
func setupIDForDeployment(client *api.Client, deploymentID int) (string, error) {
	setups, err := client.ListSetups()
	if err != nil {
		return "", fmt.Errorf("could not list setups: %w", err)
	}
	for _, s := range setups {
		if s.LeaseDeploymentID != nil && *s.LeaseDeploymentID == deploymentID {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("no setup found holding deployment #%d's lease — cannot save before terminating", deploymentID)
}
