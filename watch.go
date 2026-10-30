package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// Wait-for-supply, notify flavour. `aq watch` arms a SERVER-side availability
// watch: the orchestrator re-evaluates it on every marketplace refresh (every
// 30 to 120 seconds) and flips it to NOTIFIED when a matching offer appears.
// The watch lives on the server on purpose. A client loop over `aq gpus` dies
// with the laptop's lid; this survives it, and `--wait` / `aq watch wait` only
// POLL the one row.
//
// It is not a renting verb. A watch spends nothing and fires nothing, so it is
// deliberately absent from billableCommands. (`aq up --when-available` is the
// renting flavour and rides `up`'s guard.)

// defaultWatchPollInterval is how often a wait re-reads its watch row.
const defaultWatchPollInterval = 15 * time.Second

// Row statuses the server writes.
const (
	watchStatusWatching  = "WATCHING"
	watchStatusNotified  = "NOTIFIED"
	watchStatusExpired   = "EXPIRED"
	watchStatusCancelled = "CANCELLED"
)

// watch dispatches `aq watch`: arming is the bare form, and list / wait /
// cancel are subcommands.
func watch(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "list":
			return watchList(args[1:])
		case "wait":
			return watchWait(args[1:])
		case "cancel":
			return watchCancel(args[1:])
		}
	}
	return watchArm(args)
}

// watchFlags is every flag bare `aq watch` accepts, registered in one place so
// the top-level help can be checked against the real flag set.
type watchFlags struct {
	gpu        *string
	gpus       *int
	provider   *string
	tier       *string
	maxPrice   *float64
	expiresIn  *float64
	unreported *bool
	wait       *bool
	timeout    *time.Duration
	jsonOut    *bool
}

func registerWatchFlags(fs *flag.FlagSet) *watchFlags {
	f := &watchFlags{}
	f.gpu = fs.String("gpu", "", "GPU model to wait for, spelled as \"aq gpus\" shows it (required)")
	f.gpus = fs.Int("gpus", 1, "GPUs the offer must have. Matched EXACTLY, not \"at least\"")
	f.provider = fs.String("provider", "", "Only count offers from this provider (e.g. runpod)")
	f.tier = fs.String("tier", "", "Only count offers of this tier: datacenter or community")
	f.maxPrice = fs.Float64("max-price", 0, "Only count offers at or below this hourly price (the WHOLE offer's price, not per-GPU)")
	f.expiresIn = fs.Float64("expires-in", 0, "Give up after this many hours (server default 72, maximum 168)")
	f.unreported = fs.Bool("include-unreported", false, "Also fire on a provider that reports no stock at all, when nothing says the offer is sold out. That means worth trying, not known to be free. No email is sent: read the result with --wait or `aq watch wait`")
	f.wait = fs.Bool("wait", false, "After arming, block until the watch resolves (exit 0 match, 13 ended unmatched, 14 --timeout)")
	f.timeout = fs.Duration("timeout", 0, "With --wait: stop waiting after this long, e.g. 30s or 30m. The watch stays armed")
	f.jsonOut = fs.Bool("json", false, "Print the watch row as one JSON object on stdout")
	return f
}

// watchOptions configures runWatch. watchArm fills in the real environment;
// tests inject a credential pointing at an httptest server and a fast poll.
type watchOptions struct {
	cred         *config.Credential
	gpuModel     string
	gpuCount     int
	provider     string
	tier         string
	maxPrice     *float64
	expiresIn    *float64
	unreported   bool
	wait         bool
	timeout      time.Duration
	jsonOut      bool
	out          io.Writer
	errOut       io.Writer
	pollInterval time.Duration
	now          func() time.Time
}

func watchArm(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	f := registerWatchFlags(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return fmt.Errorf("`aq watch` takes no positional argument, got %s. Subcommands: list, wait <id>, cancel <id>", strings.Join(positional, ", "))
	}

	opts := watchOptions{
		gpuModel:   strings.TrimSpace(*f.gpu),
		gpuCount:   *f.gpus,
		provider:   strings.ToLower(strings.TrimSpace(*f.provider)),
		tier:       strings.ToLower(strings.TrimSpace(*f.tier)),
		unreported: *f.unreported,
		wait:       *f.wait,
		timeout:    *f.timeout,
		jsonOut:    *f.jsonOut,
	}
	if isFlagSet(fs, "max-price") {
		opts.maxPrice = f.maxPrice
	}
	if isFlagSet(fs, "expires-in") {
		opts.expiresIn = f.expiresIn
	}
	if err := validateWatchOptions(opts); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}
	opts.cred = cred
	opts.out, opts.errOut = os.Stdout, os.Stderr
	return runWatch(opts)
}

// validateWatchOptions refuses a request the server would refuse (or, worse,
// accept and never match), before any network call.
func validateWatchOptions(o watchOptions) error {
	if o.gpuModel == "" {
		return errors.New("--gpu is required, spelled as `aq gpus` shows it. Usage: aq watch --gpu <model> [--gpus N] [--wait]")
	}
	if o.gpuCount <= 0 {
		return fmt.Errorf("--gpus must be a positive number, got %d", o.gpuCount)
	}
	if err := validateGPUCount(o.gpuCount); err != nil {
		return err
	}
	if o.tier != "" && o.tier != "datacenter" && o.tier != "community" {
		return fmt.Errorf("--tier must be datacenter or community, got %q", o.tier)
	}
	if o.maxPrice != nil && !(*o.maxPrice > 0) {
		return fmt.Errorf("--max-price must be a positive number, got %v", *o.maxPrice)
	}
	if o.expiresIn != nil && !(*o.expiresIn > 0) {
		return fmt.Errorf("--expires-in must be a positive number of hours, got %v", *o.expiresIn)
	}
	if o.timeout < 0 {
		return fmt.Errorf("--timeout must not be negative, got %s", o.timeout)
	}
	if o.timeout > 0 && !o.wait {
		return errors.New("--timeout only applies with --wait")
	}
	return nil
}

// runWatch arms the watch and, with --wait, blocks on it.
func runWatch(opts watchOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = defaultWatchPollInterval
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	apiURL := resolveAPIURL(opts.cred)
	client := api.NewAuthed(apiURL, opts.cred.Token, opts.cred.TeamID)

	// The server matches the model name EXACTLY, so a typo would watch forever
	// in silence. Check it against the public feed first. This is only ever a
	// warning: a provider can legitimately drop a sold-out model from the feed,
	// which is exactly when someone wants a watch.
	model := opts.gpuModel
	offers, ferr := api.NewPublic(apiURL).Marketplace()
	switch {
	case ferr != nil:
		fmt.Fprintf(opts.errOut, "aq: warning: could not read the GPU feed to check the model name (%v). Arming with %q as typed; the server matches the name exactly.\n", ferr, model)
	default:
		if spelled, ok := feedSpelling(offers, model); ok {
			if spelled != model {
				fmt.Fprintf(opts.errOut, "aq: using the feed's spelling %q for %q\n", spelled, model)
			}
			model = spelled
		} else {
			fmt.Fprintf(opts.errOut, "aq: warning: no GPU named %q in the live feed. Arming anyway (a sold-out model can drop out of the feed), but the server matches the name exactly: run `aq gpus` to see the spellings.%s\n", model, didYouMean(offers, model))
		}
	}

	req := api.ArmWatchRequest{
		GPUModel:          model,
		GPUCount:          opts.gpuCount,
		MaxPricePerHour:   opts.maxPrice,
		ProviderScope:     opts.provider,
		Tier:              opts.tier,
		ExpiresInHours:    opts.expiresIn,
		IncludeUnreported: opts.unreported,
	}
	row, err := client.ArmAvailabilityWatch(req)
	if err != nil {
		return fmt.Errorf("could not arm the watch: %w", err)
	}

	// The id goes out BEFORE any waiting, so a killed wait can reattach.
	fmt.Fprintf(opts.errOut, "aq: watch %d armed. Reattach with: aq watch wait %d\n", row.ID, row.ID)

	if !opts.wait {
		if opts.jsonOut {
			return printRawJSON(opts.out, row.Raw)
		}
		printArmed(opts.out, row)
		return nil
	}
	if !opts.jsonOut {
		printArmed(opts.out, row)
	}
	return waitForWatch(watchWaitOptions{
		client:       client,
		id:           row.ID,
		last:         row,
		pollInterval: opts.pollInterval,
		timeout:      opts.timeout,
		jsonOut:      opts.jsonOut,
		out:          opts.out,
		errOut:       opts.errOut,
		now:          opts.now,
	})
}

// feedSpelling returns the feed's own spelling of want, matched
// case-insensitively and exactly (never by substring: the server does not).
func feedSpelling(offers []api.MarketplaceOffer, want string) (string, bool) {
	want = strings.TrimSpace(want)
	for _, o := range offers {
		if strings.EqualFold(o.GPUShortName, want) {
			return o.GPUShortName, true
		}
	}
	return "", false
}

// didYouMean lists up to five feed names containing want, as a suffix for a
// warning. Empty when nothing is close.
func didYouMean(offers []api.MarketplaceOffer, want string) string {
	needle := strings.ToLower(strings.TrimSpace(want))
	seen := map[string]bool{}
	var names []string
	for _, o := range offers {
		n := o.GPUShortName
		if n == "" || seen[n] || !strings.Contains(strings.ToLower(n), needle) {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	if len(names) > 5 {
		names = names[:5]
	}
	return " Close names: " + strings.Join(names, ", ") + "."
}

func printArmed(out io.Writer, row *api.AvailabilityWatch) {
	scope := "any provider"
	if row.ProviderScope != nil && *row.ProviderScope != "" {
		scope = *row.ProviderScope
	}
	price := "no price limit"
	if row.MaxPricePerHour != nil {
		price = "at or under $" + formatPrice(*row.MaxPricePerHour) + "/hr for the whole offer"
	}
	fmt.Fprintf(out, "Watch %d armed: %dx %s, %s, %s. Expires %s.\n", row.ID, row.GPUCount, row.GPUModel, scope, price, formatWhen(row.ExpiresAt))
	if row.IncludeUnreported {
		fmt.Fprintf(out, "This watch also fires on providers that report no stock, so a match means worth trying, not known to be free. No email is sent: read the result with --wait or: aq watch wait %d\n", row.ID)
		return
	}
	fmt.Fprintf(out, "The server emails you when an offer matches. Check it with: aq watch wait %d\n", row.ID)
}

// watchWaitOptions configures waitForWatch.
type watchWaitOptions struct {
	client       *api.Client
	id           int
	last         *api.AvailabilityWatch // the row as last seen, if any
	pollInterval time.Duration
	timeout      time.Duration // 0 = no client-side deadline
	jsonOut      bool
	out, errOut  io.Writer
	now          func() time.Time
}

// waitForWatch polls one watch row until it resolves.
//
// Could-not-look is not an answer. A network error or a 5xx says nothing about
// the watch, so it never ends the wait: it is reported once when it starts and
// once when it recovers, and polling continues. A 401, 403 or 404 IS an answer
// (the credential is bad, or the row is gone or not ours) and ends the wait.
// Ctrl-C is deliberately not handled: the default signal behaviour kills the
// process and the row stays armed, which is the point of server-side watches.
func waitForWatch(o watchWaitOptions) error {
	if o.pollInterval <= 0 {
		o.pollInterval = defaultWatchPollInterval
	}
	if o.now == nil {
		o.now = time.Now
	}
	var deadline time.Time
	if o.timeout > 0 {
		deadline = o.now().Add(o.timeout)
	}

	last := o.last
	failing := false
	warnedStatus := false
	for {
		row, err := o.client.GetAvailabilityWatch(o.id)
		if err != nil {
			if !isTransientWatchError(err) {
				if isNotFound(err) {
					return fmt.Errorf("watch %d not found: it does not exist or belongs to another team", o.id)
				}
				return fmt.Errorf("could not read watch %d: %w", o.id, err)
			}
			if !failing {
				fmt.Fprintf(o.errOut, "aq: cannot reach Aquanode to check watch %d (%v). Still waiting; the watch is armed on the server.\n", o.id, err)
				failing = true
			}
		} else {
			if failing {
				fmt.Fprintf(o.errOut, "aq: reconnected; watch %d is readable again.\n", o.id)
				failing = false
			}
			last = row
			switch row.Status {
			case watchStatusNotified:
				return finishNotified(o, row)
			case watchStatusExpired, watchStatusCancelled:
				if o.jsonOut {
					_ = printRawJSON(o.out, row.Raw)
				}
				return &exitError{code: exitWatchNoMatch, err: fmt.Errorf("watch %d ended with no match: it %s", o.id, strings.ToLower(row.Status))}
			case watchStatusWatching:
			default:
				if !warnedStatus {
					fmt.Fprintf(o.errOut, "aq: watch %d has status %q, which this aq does not know; still waiting.\n", o.id, row.Status)
					warnedStatus = true
				}
			}
		}

		sleep := o.pollInterval
		if o.timeout > 0 {
			remaining := deadline.Sub(o.now())
			if remaining <= 0 {
				return watchTimeout(o, last, failing)
			}
			if remaining < sleep {
				sleep = remaining
			}
		}
		time.Sleep(sleep)
	}
}

func finishNotified(o watchWaitOptions, row *api.AvailabilityWatch) error {
	if o.jsonOut {
		return printRawJSON(o.out, row.Raw)
	}
	provider := "a provider"
	if row.MatchedProvider != nil && *row.MatchedProvider != "" {
		provider = *row.MatchedProvider
	}
	price := ""
	if row.MatchedPricePerHour != nil {
		price = " at $" + formatPrice(*row.MatchedPricePerHour) + "/hr"
	}
	fmt.Fprintf(o.out, "Available: %dx %s on %s%s (watch %d).\n", row.GPUCount, row.GPUModel, provider, price, row.ID)
	return nil
}

func watchTimeout(o watchWaitOptions, last *api.AvailabilityWatch, failing bool) error {
	if o.jsonOut && last != nil {
		_ = printRawJSON(o.out, last.Raw)
	}
	if failing {
		return &exitError{code: exitWatchTimeout, err: fmt.Errorf("stopped waiting after %s and the last check could not reach Aquanode, so watch %d's state is unknown. It is probably still armed: run `aq watch wait %d` to pick it up", o.timeout, o.id, o.id)}
	}
	return &exitError{code: exitWatchTimeout, err: fmt.Errorf("stopped waiting after %s; watch %d is still WATCHING and stays armed on the server. Run `aq watch wait %d` to pick it up", o.timeout, o.id, o.id)}
}

// isTransientWatchError reports whether a poll error says nothing about the
// watch itself. Anything that is not an API answer (a network error, a gateway
// page) is transient, as are 5xx, 408 and 429. Other 4xx (401, 403, 404, ...)
// are real answers.
func isTransientWatchError(err error) bool {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	s := apiErr.Status
	return s >= 500 || s == 408 || s == 429
}

func isNotFound(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404
}

// watchWait is `aq watch wait <id> [--timeout DUR] [--json]`.
func watchWait(args []string) error {
	fs := flag.NewFlagSet("watch wait", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 0, "Stop waiting after this long, e.g. 30s or 30m. The watch stays armed")
	jsonOut := fs.Bool("json", false, "Print the final watch row as one JSON object on stdout")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	id, err := parseSingleID(positional, "aq watch wait <id>")
	if err != nil {
		return err
	}
	if *timeout < 0 {
		return fmt.Errorf("--timeout must not be negative, got %s", *timeout)
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return waitForWatch(watchWaitOptions{
		client:       newControlClient(cred),
		id:           id,
		pollInterval: defaultWatchPollInterval,
		timeout:      *timeout,
		jsonOut:      *jsonOut,
		out:          os.Stdout,
		errOut:       os.Stderr,
		now:          time.Now,
	})
}

// watchList is `aq watch list [--json]`.
func watchList(args []string) error {
	fs := flag.NewFlagSet("watch list", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Print the rows as a JSON array")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return errors.New("usage: aq watch list [--json]")
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return runWatchList(newControlClient(cred), *jsonOut, os.Stdout)
}

func runWatchList(client *api.Client, jsonOut bool, out io.Writer) error {
	rows, err := client.ListAvailabilityWatches()
	if err != nil {
		return fmt.Errorf("could not list watches: %w", err)
	}
	if jsonOut {
		raws := make([]json.RawMessage, 0, len(rows))
		for _, r := range rows {
			raws = append(raws, r.Raw)
		}
		return printJSONValue(out, raws)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "No active watches. Arm one with: aq watch --gpu <model>")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tGPU\tGPUS\tPROVIDER\tMAX $/HR\tEXPIRES")
	for _, r := range rows {
		max := "any"
		if r.MaxPricePerHour != nil {
			max = formatPrice(*r.MaxPricePerHour)
		}
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.GPUModel, r.GPUCount, scopeOrAny(r.ProviderScope), max, formatWhen(r.ExpiresAt))
	}
	return tw.Flush()
}

// watchCancel is `aq watch cancel <id>`.
func watchCancel(args []string) error {
	fs := flag.NewFlagSet("watch cancel", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	id, err := parseSingleID(positional, "aq watch cancel <id>")
	if err != nil {
		return err
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return runWatchCancel(newControlClient(cred), id, os.Stdout)
}

func runWatchCancel(client *api.Client, id int, out io.Writer) error {
	if err := client.CancelAvailabilityWatch(id); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("watch %d not found, not yours, or no longer active", id)
		}
		return fmt.Errorf("could not cancel watch %d: %w", id, err)
	}
	fmt.Fprintf(out, "Cancelled watch %d.\n", id)
	return nil
}

// parseSingleID reads exactly one positive integer id.
func parseSingleID(positional []string, usage string) (int, error) {
	if len(positional) != 1 {
		return 0, fmt.Errorf("usage: %s", usage)
	}
	id, err := strconv.Atoi(positional[0])
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a valid id (a positive number). Usage: %s", positional[0], usage)
	}
	return id, nil
}

func scopeOrAny(s *string) string {
	if s == nil || *s == "" {
		return "any"
	}
	return *s
}

// formatPrice renders a $/hr figure with at least two decimals and no
// trailing noise: 3 -> "3.00", 0.4200 -> "0.42", 2.709 -> "2.709".
func formatPrice(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	for strings.HasSuffix(s, "0") && !strings.HasSuffix(s, ".00") && len(s)-strings.Index(s, ".") > 3 {
		s = s[:len(s)-1]
	}
	return s
}

// formatWhen renders a server timestamp in UTC, or returns it unchanged when
// it does not parse (never invents a time).
func formatWhen(raw string) string {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return raw
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// printRawJSON writes one server row verbatim as a single JSON line.
func printRawJSON(out io.Writer, raw json.RawMessage) error {
	return printJSONValue(out, raw)
}

func printJSONValue(out io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(b))
	return err
}
