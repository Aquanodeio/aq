package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// startOptions configures runStart. start() fills in the real environment;
// tests call runStart directly.
type startOptions struct {
	cred     *config.Credential
	target   string // pod id (uuid) or name
	gpuModel string
	gpuCount int
	maxPrice float64
	provider string
	out      io.Writer
}

// start parses `aq start <pod> [--gpu][--max-price][--provider][--gpus]` and
// wires the real environment into runStart.
//
// Start brings a Stopped pod back onto a GPU box, on ANY matching GPU — not
// necessarily the one it last ran on (the model's own rule: "Start (on any
// GPU)"). With no filter flags it rents the cheapest offer anywhere, same as
// a bare `aq up`; the flags here narrow that search exactly like `aq up`'s
// do. Unlike `aq up`/`aq deploy`, this pod/environment/volume route needs one
// fully-resolved offer (resource + provider + sshKeyId), not a filter for
// the orchestrator to match server-side — see offer_select.go for why and
// how that offer is chosen.
//
// This replaces the old pause/resume cycle: pause always saved and
// released, and resuming meant `aq deploy --snapshot <deploymentId>` naming
// a raw deployment id. Stop (see stop.go) is now always the resumable save,
// and Start takes the pod itself — there is nothing left to route around.
func start(args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "Only start on GPUs at or below this hourly price (the whole offer's price, not per-GPU)")
	gpus := fs.Int("gpus", 0, "How many GPUs the box should have (default: 1)")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. massecompute)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("a pod is required, usage: aq start <pod>")
	}

	if err := validateGPUCount(*gpus); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runStart(startOptions{
		cred:     cred,
		target:   positional[0],
		gpuModel: *gpu,
		gpuCount: *gpus,
		maxPrice: *maxPrice,
		provider: *provider,
		out:      os.Stdout,
	})
}

// runStart resolves the target to a pod id, picks the cheapest offer
// matching the given filters, and starts the pod on it.
func runStart(opts startOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	setupID, err := resolveSetupID(client, opts.target)
	if err != nil {
		return err
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

	res, err := client.StartSetup(setupID, api.StartSetupRequest{Offer: offer})
	if err != nil {
		return fmt.Errorf("could not start %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Starting %s.\n", res.Name)
	printPodStorageSummary(out, *res)
	return nil
}
