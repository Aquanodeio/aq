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

// akashProvider is the one marketplace provider whose `price` field is
// already a flat per-GPU hourly rate — every other provider's `price` is the
// TOTAL for the whole offer (all gpuCount GPUs). Verified empirically
// against the live feed: akash H100 offers price at 2.709 regardless of gpuCount
// 1/2/3/4, and RTX5090 at 0.609 for both x1 and x8, while e.g. simplepod
// RTX3070 scales linearly with gpuCount (a total). This mirrors the
// exception website/lib/pricing.ts already carries for the same feed.
// Kept as a single named constant so the exception is greppable and cannot
// be silently dropped by a future refactor.
const akashProvider = "akash"

// isAkashFlatRate reports whether an offer's price came from the one
// provider whose feed semantics differ (see akashProvider).
func isAkashFlatRate(provider string) bool {
	return strings.EqualFold(provider, akashProvider)
}

// safeGPUCount guards the divide in perGPUHourlyRate/totalHourlyRate against
// a malformed feed row: the marketplace should never emit gpuCount <= 0, but
// if it does, treating the offer as a single GPU keeps the row visible (at a
// possibly-wrong rate) rather than dividing by zero or a negative count and
// crashing or silently dropping the offer.
func safeGPUCount(o api.MarketplaceOffer) int {
	if o.GPUCount <= 0 {
		return 1
	}
	return o.GPUCount
}

// perGPUHourlyRate normalizes an offer's price into a per-GPU hourly rate —
// the unit that is comparable across providers (see akashProvider for why
// this isn't just Price/GPUCount for everyone).
func perGPUHourlyRate(o api.MarketplaceOffer) float64 {
	if isAkashFlatRate(o.Provider) {
		return o.Price
	}
	return o.Price / float64(safeGPUCount(o))
}

// totalHourlyRate normalizes an offer's price into the whole-offer hourly
// total — the inverse normalization of perGPUHourlyRate.
func totalHourlyRate(o api.MarketplaceOffer) float64 {
	if isAkashFlatRate(o.Provider) {
		return o.Price * float64(safeGPUCount(o))
	}
	return o.Price
}

// gpusJSONOffer is what `--json` actually encodes: every raw field the API
// returned, untouched, plus one derived field. Embedding MarketplaceOffer
// (rather than copying fields) guarantees the raw contract can't drift from
// what api.Marketplace() decoded — this file must never mutate Price itself.
type gpusJSONOffer struct {
	api.MarketplaceOffer
	PricePerGPUHour float64 `json:"pricePerGpuHour"`
}

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
	summary   bool
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
	opts, err := parseGPUsOptions(args)
	if err != nil {
		return err
	}
	opts.out, opts.errOut = os.Stdout, os.Stderr
	return runGPUs(opts)
}

// parseGPUsOptions turns `aq gpus` flags into a gpusOptions, leaving the
// writers to the caller. Split out of gpus() so the no-argument gate below —
// the single decision that picks the summary view over the offer table — is
// testable without a marketplace round trip.
func parseGPUsOptions(args []string) (gpusOptions, error) {
	fs := flag.NewFlagSet("gpus", flag.ContinueOnError)
	gpu := fs.String("gpu", "", "Filter to a GPU model (substring, case-insensitive, e.g. \"B200\")")
	maxPrice := fs.Float64("max-price", 0, "Only show offers at or below this per-GPU hourly rate ($/GPU-HR, see the price columns aq gpus prints)")
	provider := fs.String("provider", "", "Restrict to a single provider (e.g. runpod)")
	region := fs.String("region", "", "Filter to a region (substring, case-insensitive)")
	jsonOut := fs.Bool("json", false, "Print the filtered offers as JSON instead of a table")
	limit := fs.Int("limit", defaultGPUsLimit, "Max rows to print (0 = all). --json returns every match unless this is set explicitly")
	if _, err := parseInterspersed(fs, args); err != nil {
		return gpusOptions{}, err
	}

	return gpusOptions{
		// A bare `aq gpus` — no flag of any kind — gets the market-shape
		// view (see printModelSummary). The moment the caller expresses ANY
		// intent, including --limit or --json, they get the full offer list
		// they asked for. Gating on "was a flag passed" rather than on the
		// value of any one flag keeps every existing invocation
		// byte-identical to what it printed before.
		summary:   fs.NFlag() == 0,
		apiURL:    resolvePublicAPIURL(),
		gpu:       *gpu,
		maxPrice:  *maxPrice,
		hasMaxPri: isFlagSet(fs, "max-price"),
		provider:  *provider,
		region:    *region,
		jsonOut:   *jsonOut,
		limit:     *limit,
		hasLimit:  isFlagSet(fs, "limit"),
	}, nil
}

// resolvePublicAPIURL picks the API base for the one command that must never
// gate on login: AQ_API_URL if set, else the built-in default. Unlike
// resolveAPIURL (down.go), this deliberately never consults a stored
// credential's APIURL — reading config.Load() at all here would make `aq
// gpus` depend on ~/.config/aq existing/parsing cleanly, which defeats the
// point of a zero-account command. The env var still outranks everything:
// an explicit environment override must beat any persisted value, so that
// pointing the CLI at a local or staging API is never silently overruled by
// whatever host a previous login happened to write to disk.
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
	// Cheapest-per-GPU-first: sorting on the raw Price would let a
	// multi-GPU Akash offer (already per-GPU) or a multi-GPU total from any
	// other provider rank by an inflated/deflated multiple of its real
	// per-GPU cost instead of its real one.
	sort.SliceStable(filtered, func(i, j int) bool {
		return perGPUHourlyRate(filtered[i]) < perGPUHourlyRate(filtered[j])
	})

	if opts.jsonOut {
		// --json is the scripting path, so the row cap that keeps the table
		// readable must not silently truncate it: an unset --limit means
		// every match. An explicitly typed --limit is still honoured —
		// ignoring a flag the caller actually passed is the worse surprise.
		out := filtered
		if opts.hasLimit && opts.limit > 0 && opts.limit < len(out) {
			out = out[:opts.limit]
		}
		jsonOffers := make([]gpusJSONOffer, len(out))
		for i, o := range out {
			jsonOffers[i] = gpusJSONOffer{MarketplaceOffer: o, PricePerGPUHour: perGPUHourlyRate(o)}
		}
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(jsonOffers); err != nil {
			return fmt.Errorf("encode offers: %w", err)
		}
		return nil
	}

	if len(filtered) == 0 {
		fmt.Fprintln(opts.out, "No offers match those filters.")
		printGPUsSignupPointer(opts.errOut)
		return nil
	}

	if opts.summary {
		printModelSummary(opts.out, filtered)
		printSummaryDrilldown(opts.errOut, filtered)
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
		fmt.Fprintf(opts.errOut, "showing %d of %d offers; use --limit 0 for all\n", len(shown), len(filtered))
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
		if opts.hasMaxPri && perGPUHourlyRate(o) > opts.maxPrice {
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
// stay readable at 100 cols: GPU, GPUS, VRAM, PROVIDER, REGION, AVAIL,
// $/GPU-HR, $/HR TOTAL. Two price columns instead of one $/HR because the
// feed's raw price unit varies by provider (see akashProvider) — printing
// both, each normalized consistently, means neither column can be misread
// as the other's unit.
func printOffers(out io.Writer, offers []api.MarketplaceOffer) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "GPU\tGPUS\tVRAM\tPROVIDER\tREGION\tAVAIL\t$/GPU-HR\t$/HR TOTAL")
	for _, o := range offers {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%d\t%.4f\t%.4f\n",
			orDash(o.GPUShortName), o.GPUCount, orDash(o.GPUMemory), orDash(o.Provider),
			orDash(o.Region), o.Available, perGPUHourlyRate(o), totalHourlyRate(o))
	}
	tw.Flush()
}

// gpuModelSummary is one row of the no-argument view: a GPU model and the
// shape of the market for it. Built only from offers that already passed
// every filter, so it can never show a model the detailed view would hide.
type gpuModelSummary struct {
	model     string
	memory    string
	cheapest  float64
	provider  string
	region    string
	providers int
	offers    int
}

// summarizeByModel collapses offers into one row per GPU model. It REQUIRES
// offers to be sorted cheapest-per-GPU-first (runGPUs does this before
// calling): that ordering is what makes the first offer seen for a model its
// cheapest, and makes the returned slice come out cheapest-model-first
// without a second sort.
//
// This is a display collapse and nothing else — no filter, no --json path,
// and no --limit path goes through here, so no caller can lose an offer to
// it. See the no-argument gate in gpus().
func summarizeByModel(offers []api.MarketplaceOffer) []gpuModelSummary {
	var rows []gpuModelSummary
	index := map[string]int{}
	seenProvider := map[string]map[string]bool{}
	for _, o := range offers {
		key := strings.ToLower(strings.TrimSpace(o.GPUShortName))
		i, ok := index[key]
		if !ok {
			// First offer for this model is its cheapest, given the sort.
			rows = append(rows, gpuModelSummary{
				model:    o.GPUShortName,
				memory:   o.GPUMemory,
				cheapest: perGPUHourlyRate(o),
				provider: o.Provider,
				region:   o.Region,
			})
			i = len(rows) - 1
			index[key] = i
			seenProvider[key] = map[string]bool{}
		}
		rows[i].offers++
		p := strings.ToLower(strings.TrimSpace(o.Provider))
		if !seenProvider[key][p] {
			seenProvider[key][p] = true
			rows[i].providers++
		}
	}
	return rows
}

// printModelSummary renders the no-argument view: every GPU model in the
// marketplace, cheapest first, with the price to beat and how many providers
// compete for it. The detailed offer table this replaces sorted the same way
// but printed rows, so its first screen was N near-identical rows of one
// provider's cheapest SKU: the cheapest consumer GPUs arrive as large blocks
// differing only in gpuCount, and the cross-provider spread that is the whole
// point of the command was never on screen.
func printModelSummary(out io.Writer, offers []api.MarketplaceOffer) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "GPU\tVRAM\tFROM $/GPU-HR\tCHEAPEST AT\tREGION\tPROVIDERS\tOFFERS")
	for _, r := range summarizeByModel(offers) {
		fmt.Fprintf(tw, "%s\t%s\t%.4f\t%s\t%s\t%d\t%d\n",
			orDash(r.model), orDash(r.memory), r.cheapest, orDash(r.provider),
			orDash(r.region), r.providers, r.offers)
	}
	tw.Flush()
}

// printSummaryDrilldown says how to get from the collapsed view to the rows
// it collapsed. Without this the summary would look like the whole feed.
func printSummaryDrilldown(errOut io.Writer, offers []api.MarketplaceOffer) {
	models := len(summarizeByModel(offers))
	fmt.Fprintf(errOut, "%d offers across %d GPU models. `aq gpus --gpu <model>` lists every offer, `--limit 0` the whole feed\n",
		len(offers), models)
}

// printGPUsSignupPointer prints a one-line next step, never a gate — `aq
// gpus` itself needs no account, and this is the only place that says what
// comes after browsing.
func printGPUsSignupPointer(errOut io.Writer) {
	fmt.Fprintln(errOut, "ready to rent one? `aq login` pairs an account, then `aq up --gpu <model>` deploys.")
}
