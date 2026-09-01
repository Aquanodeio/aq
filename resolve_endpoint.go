package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveEndpointID resolves a user-typed `<endpoint>` argument — `aq call`,
// `aq calls`, and `aq endpoint point|rm` all address endpoints by NAME — to
// the endpoint's own id.
//
// Resolution order mirrors resolveSetupID: a target that already matches an
// existing endpoint's id exactly is used as-is; otherwise it's matched
// against GET /endpoints by name, which is expected unique per team. No
// match, or more than one, is a hard error naming what was tried — this
// never guesses.
func resolveEndpointID(client *api.Client, target string) (string, error) {
	if target == "" {
		return "", errors.New("an endpoint is required")
	}

	endpoints, err := client.ListEndpoints()
	if err != nil {
		return "", fmt.Errorf("could not list endpoints: %w", err)
	}

	for _, e := range endpoints {
		if e.ID == target {
			return e.ID, nil
		}
	}

	var matches []api.Endpoint
	for _, e := range endpoints {
		if strings.EqualFold(strings.TrimSpace(e.Name), target) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no endpoint named %q", target)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d endpoints; pass the endpoint id instead", target, len(matches))
	}
}

// findEndpoint fetches the caller's endpoints and returns the one matching
// id, so callers that already have a resolved id (from resolveEndpointID)
// can get at its other fields (VersionID, Name, ...) without a dedicated GET
// /endpoints/:id endpoint.
func findEndpoint(client *api.Client, endpointID string) (*api.Endpoint, error) {
	endpoints, err := client.ListEndpoints()
	if err != nil {
		return nil, fmt.Errorf("could not list endpoints: %w", err)
	}
	for i := range endpoints {
		if endpoints[i].ID == endpointID {
			return &endpoints[i], nil
		}
	}
	return nil, fmt.Errorf("endpoint %q not found", endpointID)
}
