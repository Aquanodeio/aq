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

// jobStatusWord maps a Run's wire status to the display word both console
// and aq must show, per "Addendum 2026-09-29" of the jobs-are-jobs spec: the
// wire enum itself never changes, only what a human reads for it.
// queued/placing/provisioning/restoring collapse to one word ("Starting")
// because none of them means anything different to someone watching a job
// start — the distinction only matters to the placement/reconciliation code.
// An unrecognized status is passed through verbatim rather than guessed at:
// a future wire value must never be silently mislabeled.
func jobStatusWord(status string) string {
	switch status {
	case "queued", "placing", "provisioning", "restoring":
		return "Starting"
	case "running":
		return "Running"
	case "uploading":
		return "Saving outputs"
	case "succeeded":
		return "Succeeded"
	case "failed":
		return "Failed"
	case "cancelled":
		return "Cancelled"
	case "unservable":
		return "Couldn't run"
	default:
		return status
	}
}

// couldNotRunLegend is the exact line `aq job ls` prints once, after the
// table, iff at least one displayed row's status is "Couldn't run" (never
// per-row, never when no row qualifies): the addendum's own tooltip wording
// ("Not your code: we could not get a machine to finish this job.") is for
// console; this is the CLI's own phrasing of the same fact, given verbatim
// by the addendum for this surface.
const couldNotRunLegend = "Couldn't run: not your code, we could not get a machine to finish this job."

// unservableDisclaimer is what `aq job run` appends to an unservable run's
// final message: unlike "failed" (the owner's own code or runtime), an
// unservable run never executed the owner's workload at all, and that
// distinction must never be left for the reader to infer from the word
// alone. Wording matches the addendum's own tooltip text.
const unservableDisclaimer = "Not your code: we could not get a machine to finish this job."

// printBatchJobs renders the table: name, status, GPU, duration, cost — the
// jobs-are-jobs spec's console columns, plus ID for addressability, matching
// every other list command in this CLI (`aq ls`, `aq endpoint list`). No
// filter chips, no search: the addendum retires per-status filtering on this
// list entirely, most-recent-first is the only ordering (the server already
// returns it that way).
func printBatchJobs(out io.Writer, list []api.BatchJob, now time.Time) {
	if len(list) == 0 {
		fmt.Fprintln(out, "No jobs yet. Run `aq job run --image <ref> -- <cmd>` to start one.")
		return
	}

	fmt.Fprintf(out, "%-36s  %-24s  %-14s  %-14s  %-10s  %s\n", "ID", "NAME", "STATUS", "GPU", "DURATION", "COST")
	anyCouldNotRun := false
	for _, j := range list {
		word := jobStatusWord(j.Run.Status)
		if word == "Couldn't run" {
			anyCouldNotRun = true
		}
		fmt.Fprintf(out, "%-36s  %-24s  %-14s  %-14s  %-10s  %s\n",
			j.ID, truncate(j.Name, 24), word, formatRunGPU(j.Run), formatRunDuration(j.Run, now), formatRunCost(j.Run))
	}
	if anyCouldNotRun {
		fmt.Fprintln(out, couldNotRunLegend)
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

// runHasGPUModel reports whether any of a run's attempts ever recorded a
// GPU model -- the signal that a real box was actually rented for this run,
// as distinct from a run that never got far enough to record one.
func runHasGPUModel(run api.Run) bool {
	for _, a := range run.Attempts {
		if a.GPUModel != nil && strings.TrimSpace(*a.GPUModel) != "" {
			return true
		}
	}
	return false
}

// formatRunCost renders run.costCents, the server's own billed-cost figure
// (round(sum(billing_buckets_v2.amount_usd) * 100), computed from the
// ledger). nil is UNKNOWN -- the run has no attempt with a deployment yet,
// or every such deployment is customer-owned (unmetered) -- and renders as
// "-", never "$0.00" or "<$0.01": this CLI never computes cost itself, it
// only renders what the server sends.
//
// A real zero (exactly 0, not nil) needs a second signal to read correctly:
// on its own it is ambiguous between "billed and rounded down to nothing"
// and "never billed at all", and those must not look the same. costCents==0
// with at least one attempt recording a GPU model means a real box ran and
// billed less than half a cent, so it renders "<$0.01" (matching console's
// own behaviour for the same case). costCents==0 with no GPU model recorded
// anywhere is the degenerate case the spec does not actually expect to occur
// for a metered run -- billing never fires without a deployment, and a
// deployment always ends up with a recorded GPU model -- so it falls back to
// "-" rather than asserting a number this CLI cannot back up.
func formatRunCost(run api.Run) string {
	if run.CostCents == nil {
		return "-"
	}
	cents := *run.CostCents
	if cents == 0 {
		if runHasGPUModel(run) {
			return "<$0.01"
		}
		return "-"
	}
	return formatCents(cents)
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
