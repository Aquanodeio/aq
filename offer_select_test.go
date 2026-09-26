package main

import (
	"encoding/json"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// hyperstackOfferJSON is a marketplace offer shaped like a real hyperstack
// row: hyperstack is the provider that needs location_id (prod deployment
// 3805 400'd with "location_id is required" without it), so it is the
// fixture proving the field survives decode-then-encode, not a synthetic
// one that happens to have every field set.
const hyperstackOfferJSON = `{
	"address": "hyperstack/offer-99",
	"gpuCount": 1,
	"gpuShortName": "RTX 4090",
	"availableCpu": 8,
	"availableMemory": {"value": 32, "unit": "GB"},
	"availableStorage": {"value": 200, "unit": "GB"},
	"price": 1.0,
	"region": "US-EAST-1",
	"provider": "hyperstack",
	"location_id": "CANADA-1"
}`

// runpodOfferJSON matches start_test.go's stubMarketplaceOffer fixture: the
// shape most providers actually send, with no location_id key at all.
const runpodOfferJSON = `{
	"address": "runpod/offer-1",
	"gpuCount": 1,
	"gpuShortName": "RTX 4090",
	"availableCpu": 8,
	"availableMemory": {"value": 32, "unit": "GB"},
	"availableStorage": {"value": 200, "unit": "GB"},
	"price": 1.0,
	"region": "US-EAST-1",
	"provider": "runpod"
}`

// wireResource decodes a marketplace offer fixture, runs it through
// offerToResourceSpec (the function aq start/aq move/aq pods create all
// share to build the resource object) and re-decodes the marshaled result
// into a raw map, so the assertion reads the actual wire keys rather than
// the parsed Go value: a field renamed on either side (decode tag or
// encode tag) breaks this the same way a live mismatch would.
func wireResource(t *testing.T, offerJSON string) map[string]any {
	t.Helper()
	var offer api.MarketplaceOffer
	if err := json.Unmarshal([]byte(offerJSON), &offer); err != nil {
		t.Fatalf("decode offer fixture: %v", err)
	}
	body, err := json.Marshal(offerToResourceSpec(offer))
	if err != nil {
		t.Fatalf("marshal resource: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("re-decode resource wire: %v", err)
	}
	return wire
}

// TestOfferToResourceSpecForwardsLocationIDWhenTheOfferHasOne checks
// resource.location_id reaches the wire with the offer's own value when the
// chosen marketplace offer carries one (hyperstack): this is the field
// aq start/move/pods create were missing, causing hyperstack deployments to
// 400 with "location_id is required" (prod deployment 3805).
func TestOfferToResourceSpecForwardsLocationIDWhenTheOfferHasOne(t *testing.T) {
	wire := wireResource(t, hyperstackOfferJSON)
	got, ok := wire["location_id"]
	if !ok {
		t.Fatalf("resource.location_id is absent from the wire body; wire = %+v", wire)
	}
	if got != "CANADA-1" {
		t.Errorf("resource.location_id = %v, want CANADA-1", got)
	}
}

// TestOfferToResourceSpecOmitsLocationIDWhenTheOfferHasNone checks the key
// is ABSENT, not sent as an empty string, when the chosen offer has no
// location_id (most providers): optional means absent on this wire (D-
// pattern shared with region/gpuModel/desiredInstanceId), and a caller
// asserting the parsed value instead of the wire would miss a regression
// that sends `"location_id":""`.
func TestOfferToResourceSpecOmitsLocationIDWhenTheOfferHasNone(t *testing.T) {
	wire := wireResource(t, runpodOfferJSON)
	if got, ok := wire["location_id"]; ok {
		t.Errorf("resource.location_id must be absent when the offer has none; got %+v", got)
	}
}
