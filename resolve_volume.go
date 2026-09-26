package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveVolumeID resolves a user-typed `<name-or-id>` argument to a
// Volume's own id, the same two-step id-then-name order resolveSecret uses:
// an exact id match wins outright, otherwise it falls back to a
// case-insensitive name match against GET /volumes. No match, or more than
// one, is a hard error naming what was tried.
func resolveVolumeID(client *api.Client, target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", errors.New("a volume name or id is required")
	}

	volumes, err := client.ListVolumes()
	if err != nil {
		return "", fmt.Errorf("could not list volumes: %w", err)
	}

	for _, v := range volumes {
		if v.ID == target {
			return v.ID, nil
		}
	}

	var matches []api.Volume
	for _, v := range volumes {
		if strings.EqualFold(strings.TrimSpace(v.Name), target) {
			matches = append(matches, v)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no volume named %q", target)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d volumes; pass the volume id instead", target, len(matches))
	}
}
