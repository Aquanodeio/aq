package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveEnvironmentID resolves a user-typed `<name-or-id>` argument to a
// named Environment's own id (never a pod's), searching across all three
// groups GET /environments returns (builtin, yours, shared) the same way
// resolveSecret resolves a secret: an exact id match wins outright,
// otherwise it falls back to a case-insensitive name match. No match, or
// more than one across the combined groups, is a hard error naming what was
// tried.
func resolveEnvironmentID(client *api.Client, target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", errors.New("an environment name or id is required")
	}

	envs, err := client.ListEnvironments()
	if err != nil {
		return "", fmt.Errorf("could not list environments: %w", err)
	}
	all := append(append(append([]api.EnvironmentSummary{}, envs.Builtin...), envs.Yours...), envs.Shared...)

	for _, e := range all {
		if e.ID == target {
			return e.ID, nil
		}
	}

	var matches []api.EnvironmentSummary
	for _, e := range all {
		if strings.EqualFold(strings.TrimSpace(e.Name), target) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no environment named %q", target)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d environments; pass the environment id instead", target, len(matches))
	}
}
