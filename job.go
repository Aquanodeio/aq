package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// job dispatches `aq job <sub>` — the whole Jobs vocabulary.
//
// A GROUP rather than top-level verbs: `aq run` and `aq logs` already mean
// "push this directory to a box and run something on it" and "tail a box's
// logs". Those are daily commands, and `aq run mybox` / `aq run myjob` are the
// same string, so there is no argument shape that could disambiguate them.
func job(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq job <create|point|rm> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return jobCreate(rest)
	case "point":
		return jobPoint(rest)
	case "rm":
		return jobRemove(rest)
	case "run":
		return jobRun(rest)
	case "runs":
		return jobRuns(rest)
	case "logs":
		return jobLogs(rest)
	case "cancel":
		return jobCancel(rest)
	default:
		return fmt.Errorf("aq job: unknown subcommand %q, expected one of create, point, rm, run, runs, logs, cancel", sub)
	}
}

// jobCreateOptions configures runJobCreate. jobCreate() fills
// in the real environment; tests run runJobCreate directly.
type jobCreateOptions struct {
	cred          *config.Credential
	setupTarget   string // setup id (uuid) or name
	version       int    // the per-lineage version NUMBER to make callable
	name          string // job name (defaults to the setup's own name)
	maxInstances  int
	spendCapCents int64
	// onAlias is the `--on <alias>` value as typed, kept only for output,
	// pinnedDeploymentID is what actually goes on the wire.
	onAlias string
	// pinnedDeploymentID pins the job to a box the customer already
	// attached, instead of hardware Aquanode rents. Zero means the ordinary
	// managed path, never send it as a bare "0" or a negative number; the
	// wire key must be absent unless this is a real attached deployment id.
	pinnedDeploymentID int
	out                io.Writer
}

// jobCreate parses `aq job create <setup> <version>` and wires the
// real environment into runJobCreate.
//
// --max-instances is always required. --spend-cap-cents is required too,
// UNLESS --on pins the job to a box the customer already attached: that
// box bills nothing (Aquanode never rented it), so a spend cap on it can
// never fire and demanding one just asks for a number that means nothing.
// An job is a callable address anyone with its name can hit, handing
// one out is handing out a GPU budget, so leaving a cap unset on the
// managed (rented-hardware) path is still a hard error, not a silent
// default to unbounded.
func jobCreate(args []string) error {
	fs := flag.NewFlagSet("job create", flag.ContinueOnError)
	name := fs.String("name", "", "job name (default: the setup's own name)")
	maxInstances := fs.Int("max-instances", 0, "maximum concurrent instances this job may run (required)")
	spendCapCents := fs.Int64("spend-cap-cents", -1, "spend cap in cents before this job stops accepting runs (required unless --on is set)")
	on := fs.String("on", "", "run this job on a host you already attached with `aq attach`, instead of renting hardware")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq job create <setup> <version> --max-instances <n> [--spend-cap-cents <n>] [--on <alias>]")
	}
	setupTarget := positional[0]
	version, err := strconv.Atoi(positional[1])
	if err != nil || version <= 0 {
		return fmt.Errorf("invalid version %q; pass the version number shown by `aq save` or `aq setups` (e.g. 3 for v3)", positional[1])
	}
	if *maxInstances <= 0 {
		return errors.New("--max-instances is required and must be a positive number: a job hands out a GPU budget, so it never defaults to unbounded")
	}

	onAlias := strings.TrimSpace(*on)
	var pinnedDeploymentID int
	if onAlias != "" {
		h, err := lookupHost(onAlias)
		if err != nil {
			return err
		}
		if !h.Attached() {
			return fmt.Errorf("host %q is not attached: run `aq attach %s` first, then retry `aq job create ... --on %s`", onAlias, onAlias, onAlias)
		}
		if h.DeploymentID <= 0 {
			// Cannot happen given h.Attached() above (it requires a nonzero
			// DeploymentID), but the wire must never see a non-positive pin
			// under any circumstance, so this is asserted explicitly rather
			// than trusted.
			return fmt.Errorf("host %q has no valid attached deployment id: run `aq attach %s` again", onAlias, onAlias)
		}
		pinnedDeploymentID = h.DeploymentID
	}

	if pinnedDeploymentID == 0 {
		if *spendCapCents < 0 {
			return errors.New("--spend-cap-cents is required and must be >= 0: a job hands out a GPU budget, so it never defaults to unbounded")
		}
	} else if *spendCapCents < 0 {
		// --on pins to a box that bills nothing, so no cap was requested.
		// 0 is the only value that keeps the meaning honest: a cap that can
		// never fire is a cap of nothing spendable, never "unbounded".
		*spendCapCents = 0
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobCreate(jobCreateOptions{
		cred:               cred,
		setupTarget:        setupTarget,
		version:            version,
		name:               *name,
		maxInstances:       *maxInstances,
		spendCapCents:      *spendCapCents,
		onAlias:            onAlias,
		pinnedDeploymentID: pinnedDeploymentID,
		out:                os.Stdout,
	})
}

// runJobCreate resolves the (setup, version-number) pair to a version
// row id (same resolution `aq share` uses) and makes it callable.
func runJobCreate(opts jobCreateOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	// Never send a zero or negative pin on the wire under any circumstance,
	// the key must be absent unless it is a real attached deployment id.
	// CreateJobRequest.PinnedDeploymentID carries `omitempty` for the
	// zero case; this guards the negative case, which omitempty does not.
	if opts.pinnedDeploymentID < 0 {
		return fmt.Errorf("internal error: refusing to send a negative pinnedDeploymentId (%d)", opts.pinnedDeploymentID)
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.setupTarget)
	if err != nil {
		return err
	}
	versionRowID, err := resolveSetupVersionRowID(client, setupID, opts.version)
	if err != nil {
		return err
	}

	name := opts.name
	if name == "" {
		name = setupDisplayName(client, setupID)
	}

	ep, err := client.CreateJob(api.CreateJobRequest{
		Name:               name,
		VersionID:          versionRowID,
		MaxInstances:       opts.maxInstances,
		SpendCapCents:      opts.spendCapCents,
		PinnedDeploymentID: opts.pinnedDeploymentID,
	})
	if err != nil {
		// A pin refused server-side (a bad --on bind) already names its own
		// fix, relay it verbatim rather than burying it inside a generic
		// "could not create job" wrapper.
		if opts.pinnedDeploymentID != 0 {
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
				return errors.New(apiErr.Message)
			}
		}
		return fmt.Errorf("could not create job %q: %w", name, err)
	}

	if opts.pinnedDeploymentID != 0 {
		fmt.Fprintf(out, "✓ Created job %q → v%d (max %d instance(s), pinned to %s, bills nothing)\n",
			ep.Name, opts.version, opts.maxInstances, opts.onAlias)
	} else {
		fmt.Fprintf(out, "✓ Created job %q → v%d (max %d instance(s), spend cap %s)\n",
			ep.Name, opts.version, opts.maxInstances, formatCents(opts.spendCapCents))
	}
	return nil
}

// jobPointOptions configures runJobPoint. jobPoint() fills in
// the real environment; tests run runJobPoint directly.
type jobPointOptions struct {
	cred    *config.Credential
	target  string // job id or name
	version int    // the per-lineage version NUMBER to repoint to
	out     io.Writer
}

// jobPoint parses `aq job point <name> <version>` and wires the
// real environment into runJobPoint.
func jobPoint(args []string) error {
	fs := flag.NewFlagSet("job point", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq job point <name> <version>")
	}
	target := positional[0]
	version, err := strconv.Atoi(positional[1])
	if err != nil || version <= 0 {
		return fmt.Errorf("invalid version %q; pass the version number shown by `aq save` or `aq setups` (e.g. 3 for v3)", positional[1])
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobPoint(jobPointOptions{cred: cred, target: target, version: version, out: os.Stdout})
}

// runJobPoint repoints a job at a different version NUMBER within
// the same save lineage its current version already belongs to — the same
// command rolls forward or back, it just depends which number is passed.
//
// The repoint API and the number a user types are both scoped to a version
// NUMBER within one lineage (the same per-lineage counter `aq share` and
// `aq job create` use), but an Job only carries its current
// VersionID, not the owning setup/lineage name. So this first resolves
// VersionID → (setup id, lineage name) via GetSetupVersion, then resolves
// the typed number against THAT lineage via ListSetupVersions — the same
// two-step resolveSetupVersionRowID already does starting from a setup id
// directly, just starting from the job's live version instead.
func runJobPoint(opts jobPointOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}
	ep, err := findJob(client, jobID)
	if err != nil {
		return err
	}

	current, err := client.GetSetupVersion(ep.VersionID)
	if err != nil {
		return fmt.Errorf("could not resolve job %q's current version: %w", ep.Name, err)
	}

	versions, err := client.ListSetupVersions(current.Name)
	if err != nil {
		return fmt.Errorf("could not look up versions named %q: %w", current.Name, err)
	}
	var targetRowID int
	found := false
	for _, v := range versions {
		if v.SetupID == current.SetupID && v.Version == opts.version {
			targetRowID = v.ID
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no version %d found in %q's save lineage %q", opts.version, ep.Name, current.Name)
	}

	updated, err := client.RepointJob(jobID, api.RepointJobRequest{VersionID: targetRowID})
	if err != nil {
		return fmt.Errorf("could not repoint job %q: %w", ep.Name, err)
	}

	fmt.Fprintf(out, "✓ Repointed job %q → v%d\n", updated.Name, opts.version)
	return nil
}

// jobRemoveOptions configures runJobRemove. jobRemove() fills
// in the real environment; tests run runJobRemove directly.
type jobRemoveOptions struct {
	cred   *config.Credential
	target string // job id or name
	out    io.Writer
}

// jobRemove parses `aq job rm <name>` and wires the real
// environment into runJobRemove.
func jobRemove(args []string) error {
	fs := flag.NewFlagSet("job rm", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq job rm <name>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runJobRemove(jobRemoveOptions{cred: cred, target: target, out: os.Stdout})
}

// runJobRemove deletes a job.
func runJobRemove(opts jobRemoveOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	jobID, err := resolveJobID(client, opts.target)
	if err != nil {
		return err
	}
	ep, err := findJob(client, jobID)
	if err != nil {
		return err
	}

	if err := client.DeleteJob(jobID); err != nil {
		return fmt.Errorf("could not remove job %q: %w", ep.Name, err)
	}

	fmt.Fprintf(out, "✓ Removed job %q\n", ep.Name)
	return nil
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
