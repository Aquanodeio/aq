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
		return "", errors.New("a pod is required")
	}
	if looksLikeUUID(target) {
		return target, nil
	}

	setups, err := client.ListSetups()
	if err != nil {
		return "", fmt.Errorf("could not list pods: %w", err)
	}

	var matches []api.Setup
	for _, s := range setups {
		if strings.EqualFold(strings.TrimSpace(s.Name), target) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no pod named %q", target)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d pods; pass the pod id instead", target, len(matches))
	}
}

// findSetup fetches the caller's setups and returns the one matching id, so
// callers that already have a resolved id (from resolveSetupID or a direct
// UUID) can get at its other fields (AttachedDeploymentID, ...) without a
// dedicated GET /setups/:id endpoint.
func findSetup(client *api.Client, setupID string) (*api.Setup, error) {
	setups, err := client.ListSetups()
	if err != nil {
		return nil, fmt.Errorf("could not list pods: %w", err)
	}
	for i := range setups {
		if setups[i].ID == setupID {
			return &setups[i], nil
		}
	}
	return nil, fmt.Errorf("pod %q not found", setupID)
}

// setupDisplayName fetches a pod's own name (the wire route is still
// GET /setups, and there is no single-pod endpoint). `aq job create` uses
// this to default a job's name to its source pod's own name when none is
// given. A failed lookup must never abort the caller — it falls back to a
// generic label instead.
func setupDisplayName(client *api.Client, setupID string) string {
	setup, err := findSetup(client, setupID)
	if err != nil || setup.Name == "" {
		return fmt.Sprintf("pod-%s", setupID)
	}
	return setup.Name
}

// resolveSetupVersionRowID turns a (setup, version-NUMBER) pair a user types
// into the setup_versions table's global row id `aq job create`/`aq job
// point` need. Kept alive after the pod/environment/volume model retired
// the version share/install/fork/run routes (D11: jobs still read
// SnapshotVersion rows by this same lineage-version addressing).
//
// These are two different counters and must never be conflated: `version` is
// a per-lineage sequence that restarts at 1 for every lineage (comfyui's v1,
// v2, v3, ...), while the row id is the versions table's global
// autoincrement key. Treating the typed number as the id directly would
// address whatever row happens to have that id — almost certainly a
// different setup, quite possibly a different account's data. So this
// always resolves through the API instead of ever guessing: list every
// version row the caller can see (ListAllSetupVersions — GET /setups has no
// nested "latest version"/lineage-name field to start a name-scoped lookup
// from, see internal/api/setups.go) and pick the one row whose SetupID
// matches AND whose Version matches what the user typed. No match is a hard
// error — this never falls back to treating the number as an id.
func resolveSetupVersionRowID(client *api.Client, setupID string, version int) (int, error) {
	setup, err := findSetup(client, setupID)
	if err != nil {
		return 0, err
	}

	versions, err := client.ListAllSetupVersions()
	if err != nil {
		return 0, fmt.Errorf("could not look up versions for %q: %w", setup.Name, err)
	}
	for _, v := range versions {
		if v.SetupID == setupID && v.Version == version {
			return v.ID, nil
		}
	}
	return 0, fmt.Errorf("no version %d found for %q", version, setup.Name)
}
