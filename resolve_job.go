package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// resolveJobID resolves a user-typed `<job>` argument — `aq run`,
// `aq runs`, and `aq job point|rm` all address jobs by NAME — to
// the job's own id.
//
// Resolution order mirrors resolveSetupID: a target that already matches an
// existing job's id exactly is used as-is; otherwise it's matched
// against GET /jobs by name, which is expected unique per team. No
// match, or more than one, is a hard error naming what was tried — this
// never guesses.
func resolveJobID(client *api.Client, target string) (string, error) {
	if target == "" {
		return "", errors.New("a job is required")
	}

	jobs, err := client.ListJobs()
	if err != nil {
		return "", fmt.Errorf("could not list jobs: %w", err)
	}

	for _, e := range jobs {
		if e.ID == target {
			return e.ID, nil
		}
	}

	var matches []api.Job
	for _, e := range jobs {
		if strings.EqualFold(strings.TrimSpace(e.Name), target) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no job named %q", target)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d jobs; pass the job id instead", target, len(matches))
	}
}

// findJob fetches the caller's jobs and returns the one matching
// id, so callers that already have a resolved id (from resolveJobID)
// can get at its other fields (VersionID, Name, ...) without a dedicated GET
// /jobs/:id job.
func findJob(client *api.Client, jobID string) (*api.Job, error) {
	jobs, err := client.ListJobs()
	if err != nil {
		return nil, fmt.Errorf("could not list jobs: %w", err)
	}
	for i := range jobs {
		if jobs[i].ID == jobID {
			return &jobs[i], nil
		}
	}
	return nil, fmt.Errorf("job %q not found", jobID)
}

// latestRunID resolves a job's most recent run id — the run `aq job
// logs`/`aq job cancel` address, now that a job is 1:1 with its Run (see the
// jobs-are-jobs spec). ListRuns
// is already ordered newest-first (acceptedAt desc, orchestrator
// jobs.controller.ts:464), so the first element is it — this still resolves
// correctly for a pre-migration job carrying more than one run, picking the
// latest exactly as `aq job pull`'s latestRunWithABox already does.
func latestRunID(client *api.Client, jobID string) (string, error) {
	runs, err := client.ListRuns(jobID)
	if err != nil {
		return "", fmt.Errorf("could not list runs: %w", err)
	}
	if len(runs) == 0 {
		return "", errors.New("this job has no run yet")
	}
	return runs[0].ID, nil
}
