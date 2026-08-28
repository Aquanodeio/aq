package api

// The marketplace endpoint backs `aq gpus` — the one command in this CLI that
// works with no Aquanode account. GET /marketplace is public: it takes no
// auth headers and the orchestrator does not gate it behind x-api-key. That
// makes it the right (only) entry point for someone evaluating Aquanode
// before signing up, so this file deliberately never touches a stored
// credential.

// MemorySize is a `{value,unit}` pair as the marketplace serializes
// availableMemory/availableStorage — never a bare number, since the unit
// varies (GB seen in practice, but nothing on the wire promises it won't
// change).
type MemorySize struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
}

// MarketplaceOffer mirrors one element of GET /marketplace's data array: one
// rentable GPU configuration at one provider/region.
//
// Price is the TOTAL $/hr for the whole offer (all GpuCount GPUs) for every
// provider EXCEPT Akash, whose feed reports Price as an already-flat
// per-GPU rate (confirmed empirically: akash H100 prices at 2.709 regardless
// of gpuCount 1-4; RTX5090 at 0.609 for both x1 and x8) — see akashProvider
// in gpus.go, the one place that exception is allowed to live. For every
// other provider it scales linearly with GpuCount (runpod 1x B200 = 6.79,
// datacrunch 4x B200 = 24.44, i.e. ~6.11/GPU — a total). Any renderer must
// normalize through gpus.go's perGPUHourlyRate/totalHourlyRate rather than
// reading Price directly, or Akash and everyone else end up compared in
// different units.
type MarketplaceOffer struct {
	Address          string     `json:"address"`
	Type             string     `json:"type"`
	LocationID       string     `json:"location_id"`
	CloudType        string     `json:"cloud_type"`
	Available        int        `json:"available"`
	GPUCount         int        `json:"gpuCount"`
	GPUShortName     string     `json:"gpuShortName"`
	AvailableCPU     int        `json:"availableCpu"`
	AvailableMemory  MemorySize `json:"availableMemory"`
	AvailableStorage MemorySize `json:"availableStorage"`
	Interface        string     `json:"interface"`
	GPUVendor        string     `json:"gpuVendor"`
	GPUVendorFamily  string     `json:"gpuVendorFamily"`
	GPUArchitecture  string     `json:"gpuArchitecture"`
	GPUMemory        string     `json:"gpuMemory"`
	IsPersistent     bool       `json:"isPersistent"`
	// Price is the offer's total $/hr — see the type doc comment.
	Price      float64 `json:"price"`
	Region     string  `json:"region"`
	Provider   string  `json:"provider"`
	ProviderID string  `json:"providerId"`
	// StoragePerGpu/CpuCoresPerGpu/RamPerGpu arrive as a JSON number that is
	// sometimes fractional (a whole-node provider's resources divided evenly
	// across its GPUs, e.g. 2403.5) — float64, never int, or decoding fails
	// outright on those rows.
	StoragePerGpu  float64 `json:"storagePerGpu"`
	CpuCoresPerGpu float64 `json:"cpuCoresPerGpu"`
	RamPerGpu      float64 `json:"ramPerGpu"`
	ProviderName   string  `json:"providerName"`
}

// NewPublic returns a Client for the marketplace and any other unauthenticated
// route: no APIKey/TeamID is ever set on it, so it is impossible for a call
// made through it to trip the login gate regardless of what's on disk in
// ~/.config/aq. Callers must not call SetAuth-equivalent fields on the result.
func NewPublic(baseURL string) *Client {
	return New(baseURL)
}

// Marketplace fetches every live GPU offer across all providers — GET
// /marketplace. It sends no auth headers (see NewPublic) and requires no
// stored credential; a non-success envelope or a decode failure surfaces as a
// clear error rather than an empty list.
func (c *Client) Marketplace() ([]MarketplaceOffer, error) {
	var out []MarketplaceOffer
	if err := c.getJSON("/marketplace", &out); err != nil {
		return nil, err
	}
	return out, nil
}
