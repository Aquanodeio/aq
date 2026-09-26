package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// moveOptions configures runMove. move() fills in the real environment;
// tests call runMove directly.
type moveOptions struct {
	cred     *config.Credential
	target   string // pod id (uuid) or name
	gpuModel string
	gpuCount int
	maxPrice float64
	provider string
	out      io.Writer
}

// move parses `aq move <pod> [--gpu][--max-price][--provider][--gpus]` and
// wires the real environment into runMove.
//
// Move stops a running pod (both captures confirmed, box released only
// after) then starts it again on a different GPU matching the given filters
// (see offer_select.go for how that offer is chosen). This replaces "Change
// machine", which used to leave the source box running because it had no
// verified exit-save to lean on — Stop's guarantee here is exactly that
// verified save. If the new Start fails, the pod is left Stopped with its
// data intact, never mid-air between two boxes.
func move(args []string) error {
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, e.g. \"RTX 4090\")")
	maxPrice := fs.Float64("max-price", 0, "Only move onto GPUs at or below this hourly price (the whole offer's price, not per-GPU)")
	gpus := fs.Int("gpus", 0, "How many GPUs the box should have (default: 1)")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. massecompute)")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("a pod is required, usage: aq move <pod>")
	}

	if err := validateGPUCount(*gpus); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runMove(moveOptions{
		cred:     cred,
		target:   positional[0],
		gpuModel: *gpu,
		gpuCount: *gpus,
		maxPrice: *maxPrice,
		provider: *provider,
		out:      os.Stdout,
	})
}

// runMove resolves the target to a pod id, picks the cheapest offer matching
// the given filters, and moves the pod onto it.
func runMove(opts moveOptions) error {
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

	res, err := client.MoveSetup(setupID, api.MoveSetupRequest{Offer: offer})
	if err != nil {
		return fmt.Errorf("could not move %q: its data is safe on whatever box it was last Stopped on: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Moved %s.\n", res.Name)
	printPodStorageSummary(out, *res)
	return nil
}
