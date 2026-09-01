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

// endpoint dispatches `aq endpoint create|point|rm` — the callable-version
// commands. An endpoint is a stable, callable address in front of one setup
// version.
func endpoint(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq endpoint <create|point|rm> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return endpointCreate(rest)
	case "point":
		return endpointPoint(rest)
	case "rm":
		return endpointRemove(rest)
	default:
		return fmt.Errorf("aq endpoint: unknown subcommand %q, expected \"create\", \"point\", or \"rm\"", sub)
	}
}

// endpointCreateOptions configures runEndpointCreate. endpointCreate() fills
// in the real environment; tests call runEndpointCreate directly.
type endpointCreateOptions struct {
	cred          *config.Credential
	setupTarget   string // setup id (uuid) or name
	version       int    // the per-lineage version NUMBER to make callable
	name          string // endpoint name (defaults to the setup's own name)
	maxInstances  int
	spendCapCents int64
	// onAlias is the `--on <alias>` value as typed, kept only for output,
	// pinnedDeploymentID is what actually goes on the wire.
	onAlias string
	// pinnedDeploymentID pins the endpoint to a box the customer already
	// attached, instead of hardware Aquanode rents. Zero means the ordinary
	// managed path, never send it as a bare "0" or a negative number; the
	// wire key must be absent unless this is a real attached deployment id.
	pinnedDeploymentID int
	out                io.Writer
}

// endpointCreate parses `aq endpoint create <setup> <version>` and wires the
// real environment into runEndpointCreate.
//
// --max-instances is always required. --spend-cap-cents is required too,
// UNLESS --on pins the endpoint to a box the customer already attached: that
// box bills nothing (Aquanode never rented it), so a spend cap on it can
// never fire and demanding one just asks for a number that means nothing.
// An endpoint is a callable address anyone with its name can hit, handing
// one out is handing out a GPU budget, so leaving a cap unset on the
// managed (rented-hardware) path is still a hard error, not a silent
// default to unbounded.
func endpointCreate(args []string) error {
	fs := flag.NewFlagSet("endpoint create", flag.ContinueOnError)
	name := fs.String("name", "", "endpoint name (default: the setup's own name)")
	maxInstances := fs.Int("max-instances", 0, "maximum concurrent instances this endpoint may run (required)")
	spendCapCents := fs.Int64("spend-cap-cents", -1, "spend cap in cents before this endpoint stops accepting calls (required unless --on is set)")
	on := fs.String("on", "", "run this endpoint on a host you already attached with `aq attach`, instead of renting hardware")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq endpoint create <setup> <version> --max-instances <n> [--spend-cap-cents <n>] [--on <alias>]")
	}
	setupTarget := positional[0]
	version, err := strconv.Atoi(positional[1])
	if err != nil || version <= 0 {
		return fmt.Errorf("invalid version %q; pass the version number shown by `aq save` or `aq setups` (e.g. 3 for v3)", positional[1])
	}
	if *maxInstances <= 0 {
		return errors.New("--max-instances is required and must be a positive number: an endpoint hands out a GPU budget, so it never defaults to unbounded")
	}

	onAlias := strings.TrimSpace(*on)
	var pinnedDeploymentID int
	if onAlias != "" {
		h, err := lookupHost(onAlias)
		if err != nil {
			return err
		}
		if !h.Attached() {
			return fmt.Errorf("host %q is not attached: run `aq attach %s` first, then retry `aq endpoint create ... --on %s`", onAlias, onAlias, onAlias)
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
			return errors.New("--spend-cap-cents is required and must be >= 0: an endpoint hands out a GPU budget, so it never defaults to unbounded")
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

	return runEndpointCreate(endpointCreateOptions{
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

// runEndpointCreate resolves the (setup, version-number) pair to a version
// row id (same resolution `aq share` uses) and makes it callable.
func runEndpointCreate(opts endpointCreateOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	// Never send a zero or negative pin on the wire under any circumstance,
	// the key must be absent unless it is a real attached deployment id.
	// CreateEndpointRequest.PinnedDeploymentID carries `omitempty` for the
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

	ep, err := client.CreateEndpoint(api.CreateEndpointRequest{
		Name:               name,
		VersionID:          versionRowID,
		MaxInstances:       opts.maxInstances,
		SpendCapCents:      opts.spendCapCents,
		PinnedDeploymentID: opts.pinnedDeploymentID,
	})
	if err != nil {
		// A pin refused server-side (a bad --on bind) already names its own
		// fix, relay it verbatim rather than burying it inside a generic
		// "could not create endpoint" wrapper.
		if opts.pinnedDeploymentID != 0 {
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest {
				return errors.New(apiErr.Message)
			}
		}
		return fmt.Errorf("could not create endpoint %q: %w", name, err)
	}

	if opts.pinnedDeploymentID != 0 {
		fmt.Fprintf(out, "✓ Created endpoint %q → v%d (max %d instance(s), pinned to %s, bills nothing)\n",
			ep.Name, opts.version, opts.maxInstances, opts.onAlias)
	} else {
		fmt.Fprintf(out, "✓ Created endpoint %q → v%d (max %d instance(s), spend cap %s)\n",
			ep.Name, opts.version, opts.maxInstances, formatCents(opts.spendCapCents))
	}
	return nil
}

// endpointPointOptions configures runEndpointPoint. endpointPoint() fills in
// the real environment; tests call runEndpointPoint directly.
type endpointPointOptions struct {
	cred    *config.Credential
	target  string // endpoint id or name
	version int    // the per-lineage version NUMBER to repoint to
	out     io.Writer
}

// endpointPoint parses `aq endpoint point <name> <version>` and wires the
// real environment into runEndpointPoint.
func endpointPoint(args []string) error {
	fs := flag.NewFlagSet("endpoint point", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq endpoint point <name> <version>")
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

	return runEndpointPoint(endpointPointOptions{cred: cred, target: target, version: version, out: os.Stdout})
}

// runEndpointPoint repoints an endpoint at a different version NUMBER within
// the same save lineage its current version already belongs to — the same
// command rolls forward or back, it just depends which number is passed.
//
// The repoint API and the number a user types are both scoped to a version
// NUMBER within one lineage (the same per-lineage counter `aq share` and
// `aq endpoint create` use), but an Endpoint only carries its current
// VersionID, not the owning setup/lineage name. So this first resolves
// VersionID → (setup id, lineage name) via GetSetupVersion, then resolves
// the typed number against THAT lineage via ListSetupVersions — the same
// two-step resolveSetupVersionRowID already does starting from a setup id
// directly, just starting from the endpoint's live version instead.
func runEndpointPoint(opts endpointPointOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	endpointID, err := resolveEndpointID(client, opts.target)
	if err != nil {
		return err
	}
	ep, err := findEndpoint(client, endpointID)
	if err != nil {
		return err
	}

	current, err := client.GetSetupVersion(ep.VersionID)
	if err != nil {
		return fmt.Errorf("could not resolve endpoint %q's current version: %w", ep.Name, err)
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

	updated, err := client.RepointEndpoint(endpointID, api.RepointEndpointRequest{VersionID: targetRowID})
	if err != nil {
		return fmt.Errorf("could not repoint endpoint %q: %w", ep.Name, err)
	}

	fmt.Fprintf(out, "✓ Repointed endpoint %q → v%d\n", updated.Name, opts.version)
	return nil
}

// endpointRemoveOptions configures runEndpointRemove. endpointRemove() fills
// in the real environment; tests call runEndpointRemove directly.
type endpointRemoveOptions struct {
	cred   *config.Credential
	target string // endpoint id or name
	out    io.Writer
}

// endpointRemove parses `aq endpoint rm <name>` and wires the real
// environment into runEndpointRemove.
func endpointRemove(args []string) error {
	fs := flag.NewFlagSet("endpoint rm", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq endpoint rm <name>")
	}
	target := positional[0]

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runEndpointRemove(endpointRemoveOptions{cred: cred, target: target, out: os.Stdout})
}

// runEndpointRemove deletes an endpoint.
func runEndpointRemove(opts endpointRemoveOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	endpointID, err := resolveEndpointID(client, opts.target)
	if err != nil {
		return err
	}
	ep, err := findEndpoint(client, endpointID)
	if err != nil {
		return err
	}

	if err := client.DeleteEndpoint(endpointID); err != nil {
		return fmt.Errorf("could not remove endpoint %q: %w", ep.Name, err)
	}

	fmt.Fprintf(out, "✓ Removed endpoint %q\n", ep.Name)
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
