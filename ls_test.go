package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
)

// TestFormatRateNeverPrintsABareDollarSign is the load-bearing billing case.
// deployments.price_per_second is denominated in the PROVIDER's currency —
// Akash rows are AKT — and conversion to USD happens only when a billing
// bucket is minted. Printing "$" here would state AKT as dollars on every
// Akash box in the list.
func TestFormatRateNeverPrintsABareDollarSign(t *testing.T) {
	cases := []struct {
		name     string
		perSec   float64
		currency string
		want     string
	}{
		{"usd", 0.0001, "USD", "0.3600 USD"},
		{"akt is not dollars", 0.002, "AKT", "7.2000 AKT"},
		{"lowercase currency is normalized", 0.0001, "usd", "0.3600 USD"},
		{"unknown currency is marked, not assumed", 0.0001, "", "0.3600 ?"},
		{"no rate recorded", 0, "USD", "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatRate(c.perSec, c.currency)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			if strings.Contains(got, "$") {
				t.Fatalf("a rate must never carry a bare $ — got %q", got)
			}
		})
	}
}

// TestFormatGPUShowsTheMultiplier: an 8-GPU box must not read like a 1-GPU box
// in a list people scan for spend.
func TestFormatGPUShowsTheMultiplier(t *testing.T) {
	cases := []struct {
		model string
		count int
		want  string
	}{
		{"H100", 8, "8x H100"},
		{"H100", 1, "H100"},
		{"H100", 0, "H100"},
		{"", 4, "4x ?"},
		{"", 0, "-"},
	}
	for _, c := range cases {
		if got := formatGPU(c.model, c.count); got != c.want {
			t.Fatalf("formatGPU(%q,%d) = %q, want %q", c.model, c.count, got, c.want)
		}
	}
}

// TestFilterDeploymentsHidesClosedByDefault: `aq ls` answers "what is costing
// me money now", so a closed box is noise unless asked for.
func TestFilterDeploymentsHidesClosedByDefault(t *testing.T) {
	deps := []api.Deployment{
		{ID: 1, Status: "ACTIVE"},
		{ID: 2, Status: "CLOSED"},
		{ID: 3, Status: "FAILED"},
		{ID: 4, Status: "PROVISIONING"},
		{ID: 0, Status: "ACTIVE"}, // no usable id
	}
	live := filterDeployments(deps, false)
	if len(live) != 2 {
		t.Fatalf("want the 2 non-terminal rows with real ids, got %d: %+v", len(live), live)
	}
	if got := len(filterDeployments(deps, true)); got != 4 {
		t.Fatalf("--all wants all 4 rows with real ids, got %d", got)
	}
}

// TestFlexFloatAcceptsPrismaDecimalStrings: Prisma serializes Decimal columns
// as strings. A strict float64 field would fail the whole row's decode, taking
// the entire deployment list down with it.
func TestFlexFloatAcceptsPrismaDecimalStrings(t *testing.T) {
	cases := map[string]float64{
		`{"price_per_second":"0.00012345"}`: 0.00012345,
		`{"price_per_second":0.00012345}`:   0.00012345,
		`{"price_per_second":null}`:         0,
		`{"price_per_second":""}`:           0,
		`{"price_per_second":"garbage"}`:    0,
		`{}`:                                0,
	}
	for body, want := range cases {
		var d api.Deployment
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatalf("%s must not fail the decode: %v", body, err)
		}
		if float64(d.PricePerSecond) != want {
			t.Fatalf("%s → %v, want %v", body, float64(d.PricePerSecond), want)
		}
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct{ created, want string }{
		{"2026-08-26T11:58:30Z", "1m"},
		{"2026-08-26T09:30:00Z", "2h30m"},
		{"2026-08-24T06:00:00Z", "2d6h"},
		{"not a date", "-"},
		{"", "-"},
	}
	for _, c := range cases {
		if got := formatAge(c.created, now); got != c.want {
			t.Fatalf("formatAge(%q) = %q, want %q", c.created, got, c.want)
		}
	}
}

func TestPrintDeploymentsEmptyStates(t *testing.T) {
	var live, all bytes.Buffer
	printDeployments(&live, nil, false, time.Now())
	printDeployments(&all, nil, true, time.Now())
	if !strings.Contains(live.String(), "Nothing running") {
		t.Fatalf("got %q", live.String())
	}
	if !strings.Contains(all.String(), "No deployments yet") {
		t.Fatalf("got %q", all.String())
	}
}

// TestTruncateKeepsColumnsAligned: orchestrator-generated names run past the
// column width, and one long name misaligns every column to its right.
func TestTruncateKeepsColumnsAligned(t *testing.T) {
	if got := truncate("Tough A30 from Us-central-1", 24); len([]rune(got)) != 24 {
		t.Fatalf("want exactly 24 runes, got %d (%q)", len([]rune(got)), got)
	}
	if got := truncate("short", 24); got != "short" {
		t.Fatalf("a short name must be untouched, got %q", got)
	}
	// Rune-safe, not byte-safe: a multibyte name must not be cut mid-character.
	if got := truncate("日本語のとても長い名前です", 5); len([]rune(got)) != 5 {
		t.Fatalf("want 5 runes, got %d (%q)", len([]rune(got)), got)
	}
}
