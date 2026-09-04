package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMarketplaceDecodesOffer checks that a full-shape marketplace row (every
// field the CLI's spec confirmed present across all 804 live offers) decodes
// into MarketplaceOffer, including the nested {value,unit} size objects.
func TestMarketplaceDecodesOffer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/marketplace" {
			t.Errorf("path = %q, want /marketplace", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{"address":"secure|NVIDIA B200|1","type":"instant",`+
			`"location_id":"EU-RO-1","cloud_type":"secure","available":1,"gpuCount":1,`+
			`"gpuShortName":"B200","availableCpu":2,"availableMemory":{"value":8,"unit":"GB"},`+
			`"availableStorage":{"value":100,"unit":"GB"},"interface":"","gpuVendor":"B200",`+
			`"gpuVendorFamily":"nvidia","gpuArchitecture":"Blackwell","gpuMemory":"180GB",`+
			`"isPersistent":false,"price":6.79,"region":"EU-RO-1","provider":"runpod",`+
			`"providerId":"secure|NVIDIA B200|1","storagePerGpu":100,"cpuCoresPerGpu":2,`+
			`"ramPerGpu":8,"providerName":"runpod"}]}`)
	}))
	defer srv.Close()

	offers, err := NewPublic(srv.URL).Marketplace()
	if err != nil {
		t.Fatalf("Marketplace: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	o := offers[0]
	if o.GPUShortName != "B200" || o.Provider != "runpod" || o.Price != 6.79 || o.GPUCount != 1 {
		t.Errorf("offer decoded wrong: %+v", o)
	}
	if o.AvailableMemory.Value != 8 || o.AvailableMemory.Unit != "GB" {
		t.Errorf("availableMemory decoded wrong: %+v", o.AvailableMemory)
	}
}

// TestMarketplaceTrimsWhitespaceFromFreeTextFields pins ticket #868: vast.ai's
// geolocation-derived region shipped with a leading space (" US" instead of
// "US"), which misaligned the `aq gpus` table one column right for that row
// and would render as a distinct value from "US" anywhere region is grouped
// or filtered. Marketplace() must trim every free-text field so a future
// dirty value from any provider can't reintroduce the same class of bug.
func TestMarketplaceTrimsWhitespaceFromFreeTextFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[{"gpuShortName":" RTX2080TI ","gpuCount":1,`+
			`"provider":" vastai","region":" US","available":1,"price":0.0986,"providerName":"vastai "}]}`)
	}))
	defer srv.Close()

	offers, err := NewPublic(srv.URL).Marketplace()
	if err != nil {
		t.Fatalf("Marketplace: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("got %d offers, want 1", len(offers))
	}
	o := offers[0]
	if o.Region != "US" {
		t.Errorf("Region = %q, want trimmed %q", o.Region, "US")
	}
	if o.Provider != "vastai" {
		t.Errorf("Provider = %q, want trimmed %q", o.Provider, "vastai")
	}
	if o.ProviderName != "vastai" {
		t.Errorf("ProviderName = %q, want trimmed %q", o.ProviderName, "vastai")
	}
	if o.GPUShortName != "RTX2080TI" {
		t.Errorf("GPUShortName = %q, want trimmed %q", o.GPUShortName, "RTX2080TI")
	}
}

// TestMarketplaceSendsNoAuthHeaders is the load-bearing test for `aq gpus`
// being a genuinely no-account command: NewPublic must never attach
// x-api-key/x-team-id, even though the underlying request path (Client.do)
// would happily send them if APIKey/TeamID were set.
func TestMarketplaceSendsNoAuthHeaders(t *testing.T) {
	var gotKey, gotTeam string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotTeam = r.Header.Get("x-team-id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":[]}`)
	}))
	defer srv.Close()

	if _, err := NewPublic(srv.URL).Marketplace(); err != nil {
		t.Fatalf("Marketplace: %v", err)
	}
	if gotKey != "" {
		t.Errorf("x-api-key = %q, want empty for a public client", gotKey)
	}
	if gotTeam != "" {
		t.Errorf("x-team-id = %q, want empty for a public client", gotTeam)
	}
}

// TestMarketplaceSurfacesEnvelopeError checks that success:false surfaces a
// clear error rather than an empty offer list — a silently-empty table would
// look identical to "no offers match your filters" to a user.
func TestMarketplaceSurfacesEnvelopeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"success":false,"error":"marketplace temporarily unavailable"}`)
	}))
	defer srv.Close()

	_, err := NewPublic(srv.URL).Marketplace()
	if err == nil {
		t.Fatal("Marketplace: want error on success:false, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error = %T, want *APIError", err)
	}
	if apiErr.Message != "marketplace temporarily unavailable" {
		t.Errorf("Message = %q, want the envelope's error text", apiErr.Message)
	}
}

// TestMarketplaceSurfacesNonJSONBody checks a malformed/non-JSON body (a
// proxy 502 HTML page, or a truncated response) errors instead of decoding
// into a zero-value empty slice.
func TestMarketplaceSurfacesNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html><body>502 Bad Gateway</body></html>")
	}))
	defer srv.Close()

	_, err := NewPublic(srv.URL).Marketplace()
	if err == nil {
		t.Fatal("Marketplace: want error on a non-JSON body, got nil")
	}
}
