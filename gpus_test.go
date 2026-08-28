package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestRunGPUsSortsByPriceAscending(t *testing.T) {
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
	// Cheapest (vastai 0.35) must lead, priciest (datacrunch 24.44) must trail.
	if !strings.Contains(lines[1], "RTX 4090") {
		t.Errorf("first row = %q, want the cheapest offer (RTX 4090)", lines[1])
	}
	if !strings.Contains(lines[4], "datacrunch") {
		t.Errorf("last row = %q, want the priciest offer (datacrunch)", lines[4])
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
