package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveEnvironment resolves a user-typed `<name-or-id>` argument to a
// named Environment's own full summary (never a pod's), searching across
// all three groups GET /environments returns (builtin, yours, shared) the
// same way resolveSecret resolves a secret: an exact id match wins
// outright, otherwise it falls back to a case-insensitive name match. No
// match, or more than one across the combined groups, is a hard error
// naming what was tried.
func resolveEnvironment(client *api.Client, target string) (api.EnvironmentSummary, error) {
	if strings.TrimSpace(target) == "" {
		return api.EnvironmentSummary{}, errors.New("an environment name or id is required")
	}

	envs, err := client.ListEnvironments()
	if err != nil {
		return api.EnvironmentSummary{}, fmt.Errorf("could not list environments: %w", err)
	}
	all := append(append(append([]api.EnvironmentSummary{}, envs.Builtin...), envs.Yours...), envs.Shared...)

	for _, e := range all {
		if e.ID == target {
			return e, nil
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
		return api.EnvironmentSummary{}, fmt.Errorf("no environment named %q", target)
	case 1:
		return matches[0], nil
	default:
		return api.EnvironmentSummary{}, fmt.Errorf("%q matches %d environments; pass the environment id instead", target, len(matches))
	}
}

// resolveEnvironmentID is resolveEnvironment narrowed to just the id, for
// callers (`aq env share`/`aq env rm`) that never need the version.
func resolveEnvironmentID(client *api.Client, target string) (string, error) {
	e, err := resolveEnvironment(client, target)
	if err != nil {
		return "", err
	}
	return e.ID, nil
}

// resolveEnvironmentVersionID resolves a user-typed environment `target`
// plus an optional version NUMBER (0 meaning "latest") to the version
// ROW id `aq pods create` sends as POST /setups' environmentVersionId. A
// non-zero version is looked up against the environment's own version
// history (GET /environments/:id/versions) rather than trusted as a row id
// directly: the number a user types and the row id are different counters,
// same distinction ListSetupVersions/GetSetupVersion already draw for the
// legacy lineage.
func resolveEnvironmentVersionID(client *api.Client, target string, version int) (string, error) {
	env, err := resolveEnvironment(client, target)
	if err != nil {
		return "", err
	}
	if version == 0 {
		if env.LatestVersion == nil {
			return "", fmt.Errorf("environment %q has no versions yet", env.Name)
		}
		return env.LatestVersion.ID, nil
	}
	versions, err := client.ListEnvironmentVersions(env.ID)
	if err != nil {
		return "", fmt.Errorf("could not list versions for %q: %w", env.Name, err)
	}
	for _, v := range versions {
		if v.Version == version {
			return v.ID, nil
		}
	}
	return "", fmt.Errorf("environment %q has no version %d", env.Name, version)
}
