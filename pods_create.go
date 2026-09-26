package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// podsCreateOptions configures runPodsCreate. podsCreate() fills in the
// real environment; tests call runPodsCreate directly.
type podsCreateOptions struct {
	cred     *config.Credential
	name     string
	env      string // environment name or id (required)
	version  int    // 0 = the environment's latest version
	volume   string // an existing volume's name or id; "" with noVolume false means "new"
	noVolume bool
	gpuModel string
	gpuCount int
	maxPrice float64
	provider string
	out      io.Writer
}

// podsCreate parses `aq pods create <name> --env <name|id> [--version <n>]
// [--volume <id> | --no-volume] [--gpu][--max-price][--provider][--gpus]`
// and wires the real environment into runPodsCreate.
//
// This is the terminal equivalent of the console's New pod screen: pick an
// Environment, pick or skip a Volume, pick a machine, and launch, in one
// command instead of three separate ones. Every other pod-creating path in
// this CLI (`aq up`) goes through the older, unmanaged /deployments/up
// route and cannot start from a kept or shared Environment; this is the
// only one that can.
func podsCreate(args []string) error {
	fs := flag.NewFlagSet("pods create", flag.ContinueOnError)
	env := fs.String("env", "", "Environment to start from: a Built-in, Yours, or Shared with you name or id (required)")
	version := fs.Int("version", 0, "Use this environment version number instead of the latest")
	volume := fs.String("volume", "", "Attach an existing volume by name or id instead of creating a new one")
	noVolume := fs.Bool("no-volume", false, "Run with no volume at all; nothing under /workspace is kept (D4)")
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "Only start on GPUs at or below this hourly price (the whole offer's price, not per-GPU)")
	gpus := fs.Int("gpus", 0, "How many GPUs the box should have (default: 1)")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. massecompute)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("a name is required, usage: aq pods create <name> --env <name|id>")
	}
	if strings.TrimSpace(*env) == "" {
		return errors.New("--env is required, usage: aq pods create <name> --env <name|id>")
	}
	if *volume != "" && *noVolume {
		return errors.New("pass at most one of --volume / --no-volume")
	}

	if err := validateGPUCount(*gpus); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runPodsCreate(podsCreateOptions{
		cred:     cred,
		name:     positional[0],
		env:      *env,
		version:  *version,
		volume:   *volume,
		noVolume: *noVolume,
		gpuModel: *gpu,
		gpuCount: *gpus,
		maxPrice: *maxPrice,
		provider: *provider,
		out:      os.Stdout,
	})
}

// runPodsCreate resolves the environment (and volume, if named) to their
// wire ids, picks the cheapest matching offer, and creates the pod.
func runPodsCreate(opts podsCreateOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)

	environmentVersionID, err := resolveEnvironmentVersionID(client, opts.env, opts.version)
	if err != nil {
		return err
	}

	// volumeID is ALWAYS sent, never omitted (the wire schema requires the
	// key): nil serializes to JSON null (no volume, D4), "new" mints a fresh
	// one (the default, matching the console's New pod screen), and an
	// explicit --volume resolves to that volume's own id.
	var volumeID *string
	switch {
	case opts.noVolume:
		volumeID = nil
	case opts.volume != "":
		id, err := resolveVolumeID(client, opts.volume)
		if err != nil {
			return err
		}
		volumeID = &id
	default:
		newLiteral := "new"
		volumeID = &newLiteral
	}

	offer, err := buildOfferSelection(client, out, offerSelectFilter{
		gpuModel: opts.gpuModel,
		gpuCount: opts.gpuCount,
		maxPrice: opts.maxPrice,
		provider: opts.provider,
	})
	if err != nil {
		return err
	}

	res, err := client.CreateSetup(api.CreateSetupRequest{
		Name:                 opts.name,
		Offer:                offer,
		EnvironmentVersionID: environmentVersionID,
		VolumeID:             volumeID,
	})
	if err != nil {
		var refused *api.CreateSetupRefusedStart
		if errors.As(err, &refused) {
			// The pod is real and already listed by `aq pods`, Stopped, not
			// leaked: never retry the create (that mints a second pod), fix
			// the refusal reason and `aq start` this one instead.
			return fmt.Errorf("created %q, but it did not start (it is Stopped, not leaked; fix this and run `aq start %s`): %w", opts.name, refused.Setup.Name, refused.Err)
		}
		return fmt.Errorf("could not create pod %q: %w", opts.name, err)
	}

	fmt.Fprintf(out, "✓ Created and starting %s.\n", res.Name)
	if res.DeploymentID != nil {
		fmt.Fprintf(out, "Check its status with: aq status %d\n", *res.DeploymentID)
	}
	printPodStorageSummary(out, *res)
	return nil
}
