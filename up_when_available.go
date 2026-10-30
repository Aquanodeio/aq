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

// `aq up --when-available` is the renting flavour of wait-for-supply: it
// queues a bare box on the server, and the orchestrator fires a real create the
// moment an offer at or under the price ceiling appears, with nobody watching.
// That is why it lives behind `up`'s billable guard (prodguard.go) with no
// change, and why --max-price is mandatory here where it is optional on a
// plain `aq up`.

// whenAvailableOptions configures runUpWhenAvailable. tests inject a
// credential pointing at an httptest server.
type whenAvailableOptions struct {
	cred      *config.Credential
	gpuModel  string
	gpuCount  int
	maxPrice  float64
	provider  string
	name      string
	expiresIn *float64
	out       io.Writer
	errOut    io.Writer
}

// whenAvailableFlags are the `aq up` flags that make no sense on a queued box.
type whenAvailableFlags struct {
	comfyui, jupyter, autoStop, showSecrets bool
	warnAfter, stopAfter                    string
	hostTarget                              string // positional host:<alias>, if any
}

// validateWhenAvailable refuses, before any network call, every flag
// combination a queued box cannot honour. Queued boxes are bare boxes, and the
// server forces idle auto-stop off on a fired deployment, so an app or an
// idle policy would be silently dropped: refuse instead of pretending.
func validateWhenAvailable(gpu string, gpuCount int, maxPrice float64, f whenAvailableFlags) error {
	if f.hostTarget != "" {
		return fmt.Errorf("--when-available rents a new box when supply appears; it cannot target %s", f.hostTarget)
	}
	if strings.TrimSpace(gpu) == "" {
		return errors.New("--when-available needs --gpu <model>, spelled as `aq gpus` shows it: the server matches the model name exactly")
	}
	if !(maxPrice > 0) {
		return errors.New("--when-available needs --max-price <n>: the price ceiling is a mandatory spend rail, because this rents a box unattended, whenever supply appears. It is compared against the WHOLE offer's hourly price")
	}
	var bad []string
	if f.comfyui {
		bad = append(bad, "--comfyui")
	}
	if f.jupyter {
		bad = append(bad, "--jupyter")
	}
	if f.autoStop {
		bad = append(bad, "--auto-stop")
	}
	if f.warnAfter != "" {
		bad = append(bad, "--warn-after")
	}
	if f.stopAfter != "" {
		bad = append(bad, "--stop-after")
	}
	if f.showSecrets {
		bad = append(bad, "--show-secrets")
	}
	if len(bad) > 0 {
		return fmt.Errorf("--when-available cannot be combined with %s: a queued box is a bare box, and the server turns idle auto-stop off on a box it rents unattended (stop it yourself with `aq down`)", strings.Join(bad, ", "))
	}
	return nil
}

// pickQueueTemplate chooses the feed row the queued request is modelled on.
//
// It deliberately applies NO price filter and NO availability filter: the
// point of the request is that the offer is sold out or too dear right now, so
// a sold-out row's price must not refuse it. The server matches the model name
// and GPU count exactly and rewrites the offer's identity at fire time, so the
// row only has to exist and parse. Rows with the requested GPU count are
// preferred; otherwise the cheapest row of the model stands in (the count the
// request carries is still the user's). Reuses selectCheapestOffer for the
// ordering.
func pickQueueTemplate(offers []api.MarketplaceOffer, gpu string, gpuCount int, provider string) (api.MarketplaceOffer, error) {
	gpu = strings.TrimSpace(gpu)
	var named []api.MarketplaceOffer
	for _, o := range offers {
		if strings.EqualFold(o.GPUShortName, gpu) {
			named = append(named, o)
		}
	}
	if len(named) == 0 {
		return api.MarketplaceOffer{}, fmt.Errorf("the feed has no GPU named %q, so there is nothing to base the request on. Run `aq gpus` for the exact spellings.%s", gpu, didYouMean(offers, gpu))
	}
	f := offerSelectFilter{provider: provider}
	if _, err := selectCheapestOffer(named, f); err != nil {
		return api.MarketplaceOffer{}, fmt.Errorf("the feed has no %q offer from provider %q, so there is nothing to base the request on. Run `aq gpus --gpu %q` to see who sells it", gpu, provider, gpu)
	}
	var exact []api.MarketplaceOffer
	for _, o := range named {
		if o.GPUCount == gpuCount {
			exact = append(exact, o)
		}
	}
	if len(exact) > 0 {
		if o, err := selectCheapestOffer(exact, f); err == nil {
			return o, nil
		}
	}
	return selectCheapestOffer(named, f)
}

// runUpWhenAvailable queues the request and prints how to find and cancel it.
// It does not wait: the watch is the server's job.
func runUpWhenAvailable(opts whenAvailableOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}
	count := opts.gpuCount
	if count == 0 {
		count = 1
	}
	provider := strings.ToLower(strings.TrimSpace(opts.provider))

	apiURL := resolveAPIURL(opts.cred)
	client := api.NewAuthed(apiURL, opts.cred.Token, opts.cred.TeamID)

	// Read the feed before touching the SSH key, so a refusal registers nothing.
	offers, err := api.NewPublic(apiURL).Marketplace()
	if err != nil {
		return fmt.Errorf("could not fetch the marketplace: %w", err)
	}
	template, err := pickQueueTemplate(offers, opts.gpuModel, count, provider)
	if err != nil {
		return err
	}

	sel, err := offerSelectionFor(client, opts.out, template)
	if err != nil {
		return err
	}
	resource := sel.Resource
	resource.GPUUnits = count
	resource.Name = strings.TrimSpace(opts.name)

	row, err := client.QueueDeployment(api.QueueDeploymentRequest{
		GPUModel:            template.GPUShortName,
		GPUCount:            count,
		PriceCeilingPerHour: opts.maxPrice,
		ProviderScope:       provider,
		ExpiresInHours:      opts.expiresIn,
		Request: api.QueuedDeployBody{
			Resource: resource,
			Provider: sel.Provider,
			ServiceConfig: api.QueuedServiceConfig{
				Type:              "VMS",
				VMSlug:            "ubuntu-24-vm",
				SSHKeyID:          sel.SSHKeyID,
				SnapshotterConfig: "aquanode",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("could not queue the request: %w", err)
	}

	fmt.Fprintf(opts.out, "Queued #%d: %dx %s, %s.\n", row.ID, row.GPUCount, row.GPUModel, providerPhrase(provider))
	fmt.Fprintf(opts.out, "A bare box is rented automatically the moment an offer is at or under $%s/hr (the whole offer's price). Idle auto-stop is off on it: stop it with `aq down`.\n", formatPrice(row.PriceCeilingPerHour))
	fmt.Fprintf(opts.out, "Expires %s.\n", formatWhen(row.ExpiresAt))
	fmt.Fprintf(opts.out, "See it with `aq queue list`. Cancel with: aq queue cancel %d\n", row.ID)
	return nil
}

func providerPhrase(provider string) string {
	if provider == "" {
		return "any provider"
	}
	return provider
}

// upWhenAvailableArgs carries the parsed `aq up` flags into the
// --when-available branch.
type upWhenAvailableArgs struct {
	comfyui, jupyter, autoStop, showSecrets bool
	warnAfter, stopAfter                    string
	gpu                                     string
	gpus                                    int
	maxPrice                                float64
	provider, name                          string
	expiresIn                               float64
}

// upWhenAvailable validates the flags (every refusal happens before any
// network call), loads the credential and queues the request.
func upWhenAvailable(fs *flag.FlagSet, positional []string, a upWhenAvailableArgs) error {
	f := whenAvailableFlags{
		comfyui: a.comfyui, jupyter: a.jupyter, autoStop: a.autoStop, showSecrets: a.showSecrets,
		warnAfter: a.warnAfter, stopAfter: a.stopAfter,
	}
	if len(positional) == 1 {
		if _, ok := parseHostTarget(positional[0]); ok {
			f.hostTarget = positional[0]
		} else {
			return fmt.Errorf("`aq up` takes no deployment argument: it rents a new box; got %q", positional[0])
		}
	}
	if err := validateWhenAvailable(a.gpu, a.gpus, a.maxPrice, f); err != nil {
		return err
	}
	if err := validateGPUCount(a.gpus); err != nil {
		return err
	}
	var expires *float64
	if isFlagSet(fs, "expires-in") {
		expires = &a.expiresIn
	}
	if expires != nil && !(*expires > 0) {
		return fmt.Errorf("--expires-in must be a positive number of hours, got %v", *expires)
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return runUpWhenAvailable(whenAvailableOptions{
		cred: cred, gpuModel: a.gpu, gpuCount: a.gpus, maxPrice: a.maxPrice,
		provider: a.provider, name: a.name, expiresIn: expires,
		out: os.Stdout, errOut: os.Stderr,
	})
}
