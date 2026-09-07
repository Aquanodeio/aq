package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveSecret resolves a user-typed `<name-or-id>` argument (`aq secret
// rm`/`aq secret rotate` both address a secret this way) to the secret's
// own row, the same two-step id-then-name order resolveJobID uses: a target
// that already matches an existing secret's id exactly is used as-is;
// otherwise it's matched against the list by name, case-insensitively. No
// match, or more than one, is a hard error naming what was tried rather than
// guessing which one was meant.
func resolveSecret(client *api.Client, teamID, target string) (*api.Secret, error) {
	if strings.TrimSpace(target) == "" {
		return nil, errors.New("a secret name or id is required")
	}

	secrets, err := client.ListSecrets(teamID)
	if err != nil {
		return nil, fmt.Errorf("could not list secrets: %w", err)
	}

	for i := range secrets {
		if secrets[i].ID == target {
			return &secrets[i], nil
		}
	}

	var matches []*api.Secret
	for i := range secrets {
		if strings.EqualFold(strings.TrimSpace(secrets[i].Name), target) {
			matches = append(matches, &secrets[i])
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no secret named %q", target)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("%q matches %d secrets; pass the secret id instead", target, len(matches))
	}
}
