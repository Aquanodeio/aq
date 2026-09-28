package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
)

// jobLs parses `aq job ls` and prints the caller's batch jobs — GET
// /jobs?shape=batch, never the unfiltered list: mixing endpoints onto this
// page would defeat the shape split the same way `aq endpoint list` already
// avoids it in the other direction.
func jobLs(args []string) error {
	fs := flag.NewFlagSet("job ls", flag.ContinueOnError)
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	client := newControlClient(cred)
	list, err := client.ListBatchJobs()
	if err != nil {
		return fmt.Errorf("could not list jobs: %w", err)
	}

	printBatchJobs(os.Stdout, list, time.Now())
	return nil
}

// printBatchJobs renders the table: name, status, GPU, duration, cost — the
// jobs-are-jobs spec's console columns, plus ID for addressability, matching
// every other list command in this CLI (`aq ls`, `aq endpoint list`).
func printBatchJobs(out io.Writer, list []api.BatchJob, now time.Time) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No jobs yet. Run `aq job run --image <ref> -- <cmd>` to start one.")
		return
	}

	fmt.Fprintf(out, "%-36s  %-24s  %-11s  %-14s  %-10s  %s\n", "ID", "NAME", "STATUS", "GPU", "DURATION", "COST")
	for _, j := range list {
		status := j.Run.Status
		if status == "unservable" {
			status = "UNSERVABLE"
		}
		fmt.Fprintf(out, "%-36s  %-24s  %-11s  %-14s  %-10s  %s\n",
			j.ID, truncate(j.Name, 24), status, formatRunGPU(j.Run), formatRunDuration(j.Run, now), formatRunCostCents(j.Run.CostCents))
	}
}

// latestAttempt returns Run.Attempts' highest-ordinal entry -- the one
// actually running (or that last ran) the box, which failover may have
// moved more than once. Ordinal, not slice order: the wire makes no promise
// about array order, only that Ordinal increases with each attempt.
func latestAttempt(run api.Run) (api.RunAttempt, bool) {
	if len(run.Attempts) == 0 {
		return api.RunAttempt{}, false
	}
	best := run.Attempts[0]
	for _, a := range run.Attempts[1:] {
		if a.Ordinal > best.Ordinal {
			best = a
		}
	}
	return best, true
}

// formatRunGPU renders the GPU column from the run's latest attempt. "-"
// before any attempt has landed a box (queued, or unservable), never a
// guess at a requested-but-unconfirmed model.
func formatRunGPU(run api.Run) string {
	a, ok := latestAttempt(run)
	if !ok || a.GPUModel == nil || strings.TrimSpace(*a.GPUModel) == "" {
		return "-"
	}
	return *a.GPUModel
}

// parseRFC3339 parses a wire timestamp, returning ok=false for "" or a
// malformed value rather than a zero time.Time a caller could mistake for
// the epoch.
func parseRFC3339(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// formatRunDuration renders how long the run has been (or was) running:
// startedAt to finishedAt, or to now for a run still in flight. "-" before
// the run has ever started (queued, or unservable — it never reached a box).
func formatRunDuration(run api.Run, now time.Time) string {
	start, ok := parseRFC3339(run.StartedAt)
	if !ok {
		return "-"
	}
	end := now
	if t, ok := parseRFC3339(run.FinishedAt); ok {
		end = t
	}
	d := end.Sub(start)
	if d < 0 {
		return "-"
	}
	return formatShortDuration(d)
}

// formatShortDuration renders a duration coarsely, the same precision `aq
// ls`'s formatAge uses for the same reason: a job you are watching cares
// about minutes, not milliseconds.
func formatShortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// formatRunCostCents renders run.costCents, the server's own billed-cost
// figure (round(sum(billing_buckets_v2.amount_usd) * 100), computed from the
// ledger). nil is UNKNOWN -- the run has no attempt with a deployment yet,
// or every such deployment is customer-owned (unmetered) -- and renders as
// "-", never "$0.00": this CLI never computes cost itself, it only renders
// what the server sends.
func formatRunCostCents(costCents *int64) string {
	if costCents == nil {
		return "-"
	}
	return formatCents(*costCents)
}

// formatCents renders a cent amount as a dollar figure, e.g. 150 -> "$1.50".
func formatCents(cents int64) string {
	neg := ""
	if cents < 0 {
		neg = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s$%d.%02d", neg, cents/100, cents%100)
}
