package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// sampleMarketplaceBody is a small fixture spanning multiple providers, GPU
// models, and prices — enough to exercise every filter and the sort order.
const sampleMarketplaceBody = `{"success":true,"data":[
  {"gpuShortName":"B200","gpuCount":1,"gpuMemory":"180GB","provider":"runpod","region":"EU-RO-1","available":1,"price":6.79},
  {"gpuShortName":"B200","gpuCount":4,"gpuMemory":"180GB","provider":"datacrunch","region":"US-EAST-1","available":2,"price":24.44},
  {"gpuShortName":"RTX 4090","gpuCount":1,"gpuMemory":"24GB","provider":"vastai","region":"EU-DE-1","available":5,"price":0.35},
  {"gpuShortName":"A100","gpuCount":1,"gpuMemory":"80GB","provider":"runpod","region":"US-WEST-1","available":3,"price":1.29}
]}`

func stubMarketplace(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/marketplace" {
			t.Errorf("path = %q, want /marketplace", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleMarketplaceBody)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunGPUsSortsByPerGPUPriceAscending(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	// Header + 4 offers.
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5 (header + 4 offers):\n%s", len(lines), out.String())
	}
	// Per-GPU rates: vastai 0.35, datacrunch 24.44/4=6.11, runpod-A100 1.29,
	// runpod-B200 6.79. Cheapest (vastai 0.35) must lead, priciest per-GPU
	// (runpod B200 at 6.79/GPU, NOT datacrunch's 24.44 total) must trail —
	// this is the whole point of sorting on the normalized rate instead of
	// the raw offer-total Price.
	if !strings.Contains(lines[1], "RTX 4090") {
		t.Errorf("first row = %q, want the cheapest per-GPU offer (RTX 4090)", lines[1])
	}
	if !strings.Contains(lines[4], "runpod") || !strings.Contains(lines[4], "B200") {
		t.Errorf("last row = %q, want the priciest-per-GPU offer (runpod B200 @ 6.79/GPU)", lines[4])
	}
}

func TestRunGPUsFilterByGPUSubstringCaseInsensitive(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), gpu: "b200", limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	if strings.Contains(out.String(), "RTX 4090") || strings.Contains(out.String(), "A100") {
		t.Errorf("--gpu b200 leaked non-B200 rows:\n%s", out.String())
	}
	if strings.Count(out.String(), "B200") < 2 {
		t.Errorf("--gpu b200 should match both B200 offers:\n%s", out.String())
	}
}

func TestRunGPUsFilterByMaxPrice(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), maxPrice: 2.0, hasMaxPri: true, limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	if strings.Contains(out.String(), "B200") {
		t.Errorf("--max-price 2.0 should exclude both B200 offers (6.79, 24.44):\n%s", out.String())
	}
	if !strings.Contains(out.String(), "RTX 4090") || !strings.Contains(out.String(), "A100") {
		t.Errorf("--max-price 2.0 should keep the two cheap offers:\n%s", out.String())
	}
}

func TestRunGPUsFilterByProviderExactMatch(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), provider: "runpod", limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	if strings.Contains(out.String(), "vastai") || strings.Contains(out.String(), "datacrunch") {
		t.Errorf("--provider runpod leaked another provider's rows:\n%s", out.String())
	}
	if strings.Count(out.String(), "runpod") != 2 {
		t.Errorf("--provider runpod should keep exactly 2 rows:\n%s", out.String())
	}
}

func TestRunGPUsFilterByRegionSubstring(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), region: "eu-", limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	if !strings.Contains(out.String(), "EU-RO-1") || !strings.Contains(out.String(), "EU-DE-1") {
		t.Errorf("--region eu- should keep both EU rows:\n%s", out.String())
	}
	if strings.Contains(out.String(), "US-EAST-1") || strings.Contains(out.String(), "US-WEST-1") {
		t.Errorf("--region eu- leaked a US row:\n%s", out.String())
	}
}

func TestRunGPUsCombinedFilters(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), provider: "runpod", maxPrice: 2.0, hasMaxPri: true, limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	if !strings.Contains(out.String(), "A100") {
		t.Errorf("runpod+maxPrice2.0 should keep A100:\n%s", out.String())
	}
	if strings.Contains(out.String(), "B200") {
		t.Errorf("runpod+maxPrice2.0 should drop the 6.79 B200 offer:\n%s", out.String())
	}
}

func TestRunGPUsNoMatchesPrintsPlainMessageAndExitsClean(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), gpu: "nonexistent-model", limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: want nil error on no matches, got %v", err)
	}
	if !strings.Contains(out.String(), "No offers match") {
		t.Errorf("out = %q, want a plain no-match message", out.String())
	}
}

func TestRunGPUsLimitTruncatesAndWarns(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), limit: 2, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 { // header + 2 rows
		t.Fatalf("got %d lines, want 3 (header + 2 rows) with --limit 2:\n%s", len(lines), out.String())
	}
	if !strings.Contains(errOut.String(), "showing 2 of 4 offers") {
		t.Errorf("errOut = %q, want a loud truncation notice", errOut.String())
	}
}

func TestRunGPUsLimitZeroMeansAll(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), limit: 0, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5 (header + all 4 offers) with --limit 0:\n%s", len(lines), out.String())
	}
	if strings.Contains(errOut.String(), "showing") {
		t.Errorf("errOut = %q, must not print a truncation notice when nothing was truncated", errOut.String())
	}
}

func TestRunGPUsJSONIsParseableAndStdoutOnlyCarriesJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), jsonOut: true, limit: 1, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	var offers []api.MarketplaceOffer
	if err := json.Unmarshal(out.Bytes(), &offers); err != nil {
		t.Fatalf("--json output did not parse: %v\noutput: %s", err, out.String())
	}
	if len(offers) != 4 {
		t.Errorf("got %d offers, want all 4 (an UNSET --limit must not truncate --json): %+v", len(offers), offers)
	}
	// stdout must be pure JSON — no truncation notice or table text mixed in.
	if strings.Contains(out.String(), "showing") || strings.Contains(out.String(), "GPUS") {
		t.Errorf("--json stdout leaked non-JSON text: %s", out.String())
	}
}

// TestRunGPUsWorksWithNoCredentialFile is the load-bearing test for `aq
// gpus` being genuinely account-free: point AQ_CONFIG_DIR at an empty temp
// dir (no credentials.json at all) and confirm the command still succeeds
// and never prompts for or requires a login.
func TestRunGPUsWorksWithNoCredentialFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AQ_CONFIG_DIR", dir)
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("test setup: credentials.json unexpectedly exists")
	}

	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs with no credential file: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("runGPUs printed nothing with no credential file present")
	}
	if strings.Contains(strings.ToLower(out.String()), "login") || strings.Contains(strings.ToLower(errOut.String()), "not logged in") {
		t.Errorf("aq gpus must never prompt/require login; out=%q errOut=%q", out.String(), errOut.String())
	}
	// credentials.json must still not exist afterwards — the command must not
	// have written one either.
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(err) {
		t.Errorf("aq gpus wrote a credential file where none existed")
	}
}

// TestRunGPUsJSONAddsDerivedPerGPURateWithoutMutatingRawFields confirms
// --json's new field is additive: the raw `price`/`gpuCount` fields decode
// unchanged (the upstream contract), and a new `pricePerGpuHour` field
// carries the normalized number for scripts, correct for both an Akash row
// (unchanged) and a non-Akash row (divided).
func TestRunGPUsJSONAddsDerivedPerGPURateWithoutMutatingRawFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleMixedProviderBody)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: srv.URL, jsonOut: true, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}

	var raw []map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("--json output did not parse: %v\noutput: %s", err, out.String())
	}
	if len(raw) != 2 {
		t.Fatalf("got %d offers, want 2", len(raw))
	}
	for _, o := range raw {
		provider, _ := o["provider"].(string)
		price, _ := o["price"].(float64)
		gpuCount, _ := o["gpuCount"].(float64)
		derived, ok := o["pricePerGpuHour"].(float64)
		if !ok {
			t.Fatalf("offer %v missing pricePerGpuHour field", o)
		}
		want := price
		if !isAkashFlatRate(provider) {
			want = price / gpuCount
		}
		if derived != want {
			t.Errorf("provider %q: pricePerGpuHour = %v, want %v (raw price=%v unchanged)", provider, derived, want, price)
		}
		if price != o["price"] {
			t.Errorf("raw price field mutated for provider %q", provider)
		}
	}
}

func TestResolvePublicAPIURLLetsTheEnvOverrideWin(t *testing.T) {
	t.Setenv("AQ_API_URL", "http://localhost:11080/api/v1")
	if got := resolvePublicAPIURL(); got != "http://localhost:11080/api/v1" {
		t.Errorf("resolvePublicAPIURL() = %q, want the env override", got)
	}
}

func TestResolvePublicAPIURLDefaultsWithNoEnv(t *testing.T) {
	t.Setenv("AQ_API_URL", "")
	if got := resolvePublicAPIURL(); got == "" {
		t.Error("resolvePublicAPIURL() = \"\", want the built-in default")
	}
}

func TestRunGPUsEnvelopeErrorSurfacesClearly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"success":false,"error":"marketplace down"}`)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: srv.URL, limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err == nil {
		t.Fatal("runGPUs: want an error when the marketplace envelope reports failure")
	}
	if !strings.Contains(err.Error(), "marketplace down") {
		t.Errorf("error = %v, want it to carry the envelope's message", err)
	}
}

// TestPerGPUHourlyRateDoesNotDivideAkashButDividesEveryoneElse is the
// regression guard for ticket #787: Akash's marketplace price is already a
// flat per-GPU rate (unlike every other provider, whose price is the whole
// offer's total), so a multi-GPU Akash offer must come back unchanged while
// a multi-GPU non-Akash offer must be divided by its gpuCount.
func TestPerGPUHourlyRateDoesNotDivideAkashButDividesEveryoneElse(t *testing.T) {
	akash := api.MarketplaceOffer{Provider: "akash", GPUCount: 4, Price: 2.709}
	if got := perGPUHourlyRate(akash); got != 2.709 {
		t.Errorf("perGPUHourlyRate(akash x4 @ 2.709) = %v, want 2.709 unchanged (akash price is already per-GPU)", got)
	}
	if got := totalHourlyRate(akash); got != 2.709*4 {
		t.Errorf("totalHourlyRate(akash x4 @ 2.709) = %v, want %v (per-GPU * gpuCount)", got, 2.709*4)
	}

	// Case-insensitive provider match, since the feed's casing isn't
	// guaranteed to be lowercase everywhere.
	akashMixedCase := api.MarketplaceOffer{Provider: "Akash", GPUCount: 8, Price: 0.609}
	if got := perGPUHourlyRate(akashMixedCase); got != 0.609 {
		t.Errorf("perGPUHourlyRate(Akash x8 @ 0.609) = %v, want 0.609 unchanged", got)
	}

	simplepod := api.MarketplaceOffer{Provider: "simplepod", GPUCount: 6, Price: 0.30}
	if got := perGPUHourlyRate(simplepod); math.Abs(got-0.05) > 1e-9 {
		t.Errorf("perGPUHourlyRate(simplepod x6 @ 0.30 total) = %v, want 0.05 (0.30 / 6)", got)
	}
	if got := totalHourlyRate(simplepod); got != 0.30 {
		t.Errorf("totalHourlyRate(simplepod x6 @ 0.30 total) = %v, want 0.30 unchanged", got)
	}

	datacrunch := api.MarketplaceOffer{Provider: "datacrunch", GPUCount: 4, Price: 24.44}
	if got := perGPUHourlyRate(datacrunch); got != 6.11 {
		t.Errorf("perGPUHourlyRate(datacrunch x4 @ 24.44 total) = %v, want 6.11 (24.44 / 4)", got)
	}
}

// TestPerGPUHourlyRateGuardsNonPositiveGPUCount pins the divide-by-zero
// guard: a malformed feed row with gpuCount <= 0 must not panic or produce
// Inf/NaN — it is treated as a single GPU so the row stays visible instead
// of vanishing or crashing `aq gpus`.
func TestPerGPUHourlyRateGuardsNonPositiveGPUCount(t *testing.T) {
	for _, count := range []int{0, -1, -8} {
		o := api.MarketplaceOffer{Provider: "runpod", GPUCount: count, Price: 5.0}
		if got := perGPUHourlyRate(o); got != 5.0 {
			t.Errorf("perGPUHourlyRate(gpuCount=%d, price=5.0) = %v, want 5.0 (treated as 1 GPU)", count, got)
		}
		if got := totalHourlyRate(o); got != 5.0 {
			t.Errorf("totalHourlyRate(gpuCount=%d, price=5.0) = %v, want 5.0 (treated as 1 GPU)", count, got)
		}

		akashO := api.MarketplaceOffer{Provider: "akash", GPUCount: count, Price: 2.709}
		if got := totalHourlyRate(akashO); got != 2.709 {
			t.Errorf("totalHourlyRate(akash, gpuCount=%d, price=2.709) = %v, want 2.709 (treated as 1 GPU)", count, got)
		}
	}
}

// sampleAkashUndersellsBody has an Akash multi-GPU offer that is nominally
// the cheapest raw Price in the set, but is NOT the cheapest per-GPU: the
// runpod row below costs less per GPU once both are normalized. Before
// ticket #787's fix, sorting on raw Price let the Akash row (which is
// already per-GPU, so its "total" reads deceptively low relative to
// multi-GPU offers whose Price is a true total) rank first.
const sampleMixedProviderBody = `{"success":true,"data":[
  {"gpuShortName":"H100","gpuCount":4,"gpuMemory":"80GB","provider":"akash","region":"US-EAST-1","available":1,"price":2.709},
  {"gpuShortName":"H100","gpuCount":1,"gpuMemory":"80GB","provider":"runpod","region":"US-WEST-1","available":1,"price":1.50}
]}`

// TestRunGPUsDefaultOrderRanksCheaperPerGPUAboveNominallyCheaperAkash pins
// the actual bug from ticket #787: with the Akash offer's raw Price (2.709)
// lower than runpod's raw Price (1.50), a naive total-Price sort would rank
// Akash first — even though Akash's 2.709 is ALREADY per-GPU while runpod's
// 1.50 is a single-GPU total, so runpod (1.50/GPU) is in fact cheaper than
// Akash (2.709/GPU). The default view must lead with runpod.
func TestRunGPUsDefaultOrderRanksCheaperPerGPUAboveNominallyCheaperAkash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleMixedProviderBody)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: srv.URL, limit: defaultGPUsLimit, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header + 2 offers):\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[1], "runpod") {
		t.Errorf("first (cheapest-per-GPU) row = %q, want runpod (1.50/GPU beats akash's 2.709/GPU)", lines[1])
	}
	if !strings.Contains(lines[2], "akash") {
		t.Errorf("second row = %q, want akash", lines[2])
	}
}

// TestRunGPUsJSONHonoursAnExplicitLimit pins the other half of the --json
// row-cap rule: the table's default cap never truncates a scripted --json
// read, but a --limit the caller actually typed is obeyed there too.
// Silently discarding an explicit flag is the worse of the two surprises.
func TestRunGPUsJSONHonoursAnExplicitLimit(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runGPUs(gpusOptions{apiURL: stubMarketplace(t), jsonOut: true, limit: 2, hasLimit: true, out: &out, errOut: &errOut})
	if err != nil {
		t.Fatalf("runGPUs: %v", err)
	}
	var offers []api.MarketplaceOffer
	if err := json.Unmarshal(out.Bytes(), &offers); err != nil {
		t.Fatalf("--json output did not parse: %v\noutput: %s", err, out.String())
	}
	if len(offers) != 2 {
		t.Fatalf("got %d offers, want 2 — an explicit --limit must apply to --json", len(offers))
	}
	// Still cheapest-first after the cut.
	if offers[0].Price > offers[1].Price {
		t.Errorf("explicit --limit sliced before sorting: %v then %v", offers[0].Price, offers[1].Price)
	}
}
