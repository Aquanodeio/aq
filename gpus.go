package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// defaultGPUsLimit bounds how many rows `aq gpus` prints without --limit. The
// marketplace runs ~800 offers deep; dumping all of them by default would
// flood the terminal on the very first thing a no-account visitor runs.
const defaultGPUsLimit = 20

// gpusOptions configures runGPUs. gpus() fills in the real environment; tests
// inject a base URL and buffer writers.
type gpusOptions struct {
	apiURL    string
	gpu       string
	maxPrice  float64
	hasMaxPri bool
	provider  string
	region    string
	jsonOut   bool
	limit     int
	hasLimit  bool
	out       io.Writer
	errOut    io.Writer
}

// gpus parses `aq gpus` and wires the real environment into runGPUs.
//
// This is the CLI's one command that works with no Aquanode account: it
// never loads or requires ~/.config/aq, and the client it builds sends no
// auth headers (api.NewPublic). See resolvePublicAPIURL for why it must not
// read a stored credential's APIURL the way every other command does.
func gpus(args []string) error {
	fs := flag.NewFlagSet("gpus", flag.ContinueOnError)
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, case-insensitive, e.g. \"B200\")")
	maxPrice := fs.Float64("max-price", 0, "Only show offers at or below this hourly price (offer total, not per-GPU)")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. runpod)")
	region := fs.String("region", "", "Filter to a region (substring, case-insensitive)")
	jsonOut := fs.Bool("json", false, "Print the filtered offers as JSON instead of a table")
	limit := fs.Int("limit", defaultGPUsLimit, "Max rows to print (0 = all). --json returns every match unless this is set explicitly")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	return runGPUs(gpusOptions{
		apiURL:    resolvePublicAPIURL(),
		gpu:       *gpu,
		maxPrice:  *maxPrice,
		hasMaxPri: isFlagSet(fs, "max-price"),
		provider:  *provider,
		region:    *region,
		jsonOut:   *jsonOut,
		limit:     *limit,
		hasLimit:  isFlagSet(fs, "limit"),
		out:       os.Stdout,
		errOut:    os.Stderr,
	})
}

// resolvePublicAPIURL picks the API base for the one command that must never
// gate on login: AQ_API_URL if set, else the built-in default. Unlike
// resolveAPIURL (down.go), this deliberately never consults a stored
// credential's APIURL — reading config.Load() at all here would make `aq
// gpus` depend on ~/.config/aq existing/parsing cleanly, which defeats the
// point of a zero-account command. The env var still outranks everything,
// same rule as gotcha #6 in CLAUDE.md.
func resolvePublicAPIURL() string {
	if v := config.APIURLOverride(); v != "" {
		return v
	}
	return config.DefaultAPIURL
}

// isFlagSet reports whether a flag was explicitly passed, so --max-price 0
// (a real, if unusual, filter) can be told apart from "the flag was never
// given".
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// runGPUs fetches the marketplace and prints the filtered, sorted result.
func runGPUs(opts gpusOptions) error {
	client := api.NewPublic(opts.apiURL)
	offers, err := client.Marketplace()
	if err != nil {
		return fmt.Errorf("could not fetch the marketplace: %w", err)
	}

	filtered := filterOffers(offers, opts)
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Price < filtered[j].Price })

	if opts.jsonOut {
		// --json is the scripting path, so the row cap that keeps the table
		// readable must not silently truncate it: an unset --limit means
		// every match. An explicitly typed --limit is still honoured —
		// ignoring a flag the caller actually passed is the worse surprise.
		out := filtered
		if opts.hasLimit && opts.limit > 0 && opts.limit < len(out) {
			out = out[:opts.limit]
		}
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fmt.Errorf("encode offers: %w", err)
		}
		return nil
	}

	if len(filtered) == 0 {
		fmt.Fprintln(opts.out, "No offers match those filters.")
		printGPUsSignupPointer(opts.errOut)
		return nil
	}

	shown := filtered
	limit := opts.limit
	if limit > 0 && limit < len(filtered) {
		shown = filtered[:limit]
	}

	printOffers(opts.out, shown)

	if limit > 0 && limit < len(filtered) {
		fmt.Fprintf(opts.errOut, "showing %d of %d offers — use --limit 0 for all\n", len(shown), len(filtered))
	}
	printGPUsSignupPointer(opts.errOut)
	return nil
}

// filterOffers applies every --gpu/--max-price/--provider/--region filter.
// Each is independent and optional; an unset filter matches everything.
func filterOffers(offers []api.MarketplaceOffer, opts gpusOptions) []api.MarketplaceOffer {
	var out []api.MarketplaceOffer
	gpu := strings.ToLower(strings.TrimSpace(opts.gpu))
	provider := strings.ToLower(strings.TrimSpace(opts.provider))
	region := strings.ToLower(strings.TrimSpace(opts.region))
	for _, o := range offers {
		if gpu != "" && !strings.Contains(strings.ToLower(o.GPUShortName), gpu) {
			continue
		}
		if opts.hasMaxPri && o.Price > opts.maxPrice {
			continue
		}
		if provider != "" && strings.ToLower(o.Provider) != provider {
			continue
		}
		if region != "" && !strings.Contains(strings.ToLower(o.Region), region) {
			continue
		}
		out = append(out, o)
	}
	return out
}

// printOffers renders the table via tabwriter, columns kept short enough to
// stay readable at 100 cols: GPU, GPUS, VRAM, PROVIDER, REGION, AVAIL, $/HR.
func printOffers(out io.Writer, offers []api.MarketplaceOffer) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "GPU\tGPUS\tVRAM\tPROVIDER\tREGION\tAVAIL\t$/HR")
	for _, o := range offers {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\t%.4f\n",
			orDash(o.GPUShortName), o.GPUCount, orDash(o.GPUMemory), orDash(o.Provider),
			orDash(o.Region), o.Available, o.Price)
	}
	tw.Flush()
}

// printGPUsSignupPointer prints a one-line next step, never a gate — `aq
// gpus` itself needs no account, and this is the only place that says what
// comes after browsing.
func printGPUsSignupPointer(errOut io.Writer) {
	fmt.Fprintln(errOut, "ready to rent one? `aq login` pairs an account, then `aq up --gpu <model>` deploys.")
}
