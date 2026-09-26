package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// boxDefaultCPU/Memory/StorageGB are the last-resort resource sizes used only
// when a marketplace offer's own reported number is unusable (zero, absent,
// or an unrecognized unit) — mirroring the console's own `BOX` fallback
// (`app/pods/new/page.tsx`: `{cpu: 4, memory: 32, storage: 100}`), since
// `resource.cpu`/`.memory`/`.storage` are required, non-nullable fields on
// the wire and omitting one is not an option.
const (
	boxDefaultCPU       = 4
	boxDefaultMemoryGB  = 32
	boxDefaultStorageGB = 100
)

// offerSelectFilter narrows the marketplace search `aq start`/`aq move` run
// client-side before building a full OfferSelection — this model's
// /setups/:id/{start,move} takes one ALREADY-CHOSEN offer, not a filter the
// orchestrator resolves server-side the way `aq up`/`aq deploy` do.
type offerSelectFilter struct {
	gpuModel string
	gpuCount int
	maxPrice float64
	provider string
}

// selectCheapestOffer filters the live marketplace by f and returns the
// cheapest matching offer by TOTAL hourly price — matching `aq up`/`aq
// deploy`'s own --max-price convention ("the WHOLE offer's price, not
// per-GPU"), not `aq gpus`'s per-GPU-rate convention, since a caller renting
// here is billed the total. --gpus matches "at least this many", the same
// semantics `aq up`'s own flag documents.
func selectCheapestOffer(offers []api.MarketplaceOffer, f offerSelectFilter) (api.MarketplaceOffer, error) {
	gpu := strings.ToLower(strings.TrimSpace(f.gpuModel))
	provider := strings.ToLower(strings.TrimSpace(f.provider))

	var matches []api.MarketplaceOffer
	for _, o := range offers {
		if gpu != "" && !strings.Contains(strings.ToLower(o.GPUShortName), gpu) {
			continue
		}
		if provider != "" && strings.ToLower(o.Provider) != provider {
			continue
		}
		if f.gpuCount > 0 && o.GPUCount < f.gpuCount {
			continue
		}
		if f.maxPrice > 0 && totalHourlyRate(o) > f.maxPrice {
			continue
		}
		matches = append(matches, o)
	}
	if len(matches) == 0 {
		return api.MarketplaceOffer{}, fmt.Errorf("no offer matches those filters; try `aq gpus` to see what's available")
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return totalHourlyRate(matches[i]) < totalHourlyRate(matches[j])
	})
	return matches[0], nil
}

// toGB converts a MemorySize into GB, mirroring the console's own `toGB`
// (`lib/utils.ts`) exactly: GB unchanged, TB*1024, MB/1024, any other unit
// UNKNOWN (nil) rather than guessed at — a value we can't convert must never
// silently become 0 or the raw number in the wrong unit.
func toGB(m api.MemorySize) *float64 {
	if !(m.Value > 0) {
		return nil
	}
	var gb float64
	switch m.Unit {
	case "GB":
		gb = m.Value
	case "TB":
		gb = m.Value * 1024
	case "MB":
		gb = m.Value / 1024
	default:
		return nil
	}
	return &gb
}

// offerToResourceSpec builds the `resource` object a Start/Move offer needs
// from a chosen marketplace offer — the same fields the console's
// `transformToBackendFormat` derives from a `chosenOffer`
// (`lib/utils/deployment-config-builder.ts`), field for field:
// desiredInstanceId <- offer.address, location_id <- offer.location_id,
// gpuUnits <- offer.gpuCount, gpuModel <- offer.gpuShortName. CPU/memory/
// storage fall back to the box default only when the offer's own number is
// unusable (see toGB) — sending the offer's real spec is what the console
// itself does for every provider except akash's own flat request, which aq
// has no equivalent per-provider table for and does not attempt to replicate.
func offerToResourceSpec(o api.MarketplaceOffer) api.ResourceSpec {
	cpu := o.AvailableCPU
	if cpu <= 0 {
		cpu = boxDefaultCPU
	}

	memoryGB := boxDefaultMemoryGB
	if gb := toGB(o.AvailableMemory); gb != nil {
		memoryGB = int(*gb)
	}

	storageGB := boxDefaultStorageGB
	if gb := toGB(o.AvailableStorage); gb != nil {
		storageGB = int(*gb)
	}

	return api.ResourceSpec{
		CPU:               cpu,
		Memory:            fmt.Sprintf("%dGi", memoryGB),
		Storage:           fmt.Sprintf("%dGi", storageGB),
		GPUUnits:          o.GPUCount,
		GPUModel:          o.GPUShortName,
		DesiredInstanceID: o.Address,
		Region:            normalizeOfferRegion(o.Region),
		LocationID:        o.LocationID,
	}
}

// regionPlaceholders mirrors the console's own REGION_PLACEHOLDERS
// (`lib/utils/deployment-config-builder.ts`) and the orchestrator's
// REGION_PLACEHOLDERS in deployment.repository.ts: every spelling a
// provider's feed has been observed sending that means "no region", folded
// to an ABSENT key rather than stored as a placeholder string. Kept in sync
// by hand across all three repos, same as the two existing copies already
// are.
var regionPlaceholders = map[string]bool{
	"":          true,
	"n/a":       true,
	"na":        true,
	"none":      true,
	"null":      true,
	"undefined": true,
	"unknown":   true,
}

// normalizeOfferRegion trims and folds a placeholder region to "" so the
// caller's `omitempty` drops the key entirely, matching the console's
// normalizeRegion.
func normalizeOfferRegion(region string) string {
	trimmed := strings.TrimSpace(region)
	if regionPlaceholders[strings.ToLower(trimmed)] {
		return ""
	}
	return trimmed
}

// buildOfferSelection resolves the cheapest offer matching f and turns it
// into the OfferSelection Start/Move need, including a real SSH key (minted
// if the caller has none, the same ensureSSHKey up.go/deploy.go already
// use).
func buildOfferSelection(client *api.Client, out io.Writer, f offerSelectFilter) (api.OfferSelection, error) {
	offers, err := client.Marketplace()
	if err != nil {
		return api.OfferSelection{}, fmt.Errorf("could not fetch the marketplace: %w", err)
	}
	offer, err := selectCheapestOffer(offers, f)
	if err != nil {
		return api.OfferSelection{}, err
	}

	sshKeyID, err := ensureSSHKey(client, out)
	if err != nil {
		return api.OfferSelection{}, err
	}

	return api.OfferSelection{
		Resource: offerToResourceSpec(offer),
		Provider: api.ProviderSpec{Name: strings.ToLower(strings.TrimSpace(offer.Provider))},
		SSHKeyID: sshKeyID,
	}, nil
}
