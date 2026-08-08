package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func TestRunIdleStatusRendersActive(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/4242/idle-policy", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"warnAfterMinutes":        30,
			"actAfterMinutes":         60,
			"gpuIdleThresholdPercent": 5,
			"autoStopEnabled":         true,
			"state":                   "ACTIVE",
			"idleMinutes":             0,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runIdleStatus(idleStatusOptions{cred: cred, target: "4242", out: &out}); err != nil {
		t.Fatalf("runIdleStatus error: %v", err)
	}
	got := out.String()
	for _, want := range []string{"ACTIVE", "30m", "1h", "enabled", "5%"} {
		if !strings.Contains(got, want) {
			t.Errorf("idle status output missing %q; got:\n%s", want, got)
		}
	}
}

func TestRunIdleStatusRendersIdleWarnWithMinutes(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/4242/idle-policy", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"warnAfterMinutes":        30,
			"actAfterMinutes":         60,
			"gpuIdleThresholdPercent": 5,
			"autoStopEnabled":         true,
			"state":                   "IDLE_WARN",
			"idleMinutes":             18,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runIdleStatus(idleStatusOptions{cred: cred, target: "4242", out: &out}); err != nil {
		t.Fatalf("runIdleStatus error: %v", err)
	}
	if !strings.Contains(out.String(), "18 min") {
		t.Errorf("expected idle minutes in output; got:\n%s", out.String())
	}
}

// TestRunIdleStatusRendersUnknownHonestly pins the honesty constraint: UNKNOWN
// means the box reported no usable data, so it must render as its own state and
// never silently as ACTIVE or IDLE — either would be a lie the user could act on.
func TestRunIdleStatusRendersUnknownHonestly(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/909/idle-policy", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"warnAfterMinutes":        30,
			"actAfterMinutes":         60,
			"gpuIdleThresholdPercent": 5,
			"autoStopEnabled":         false,
			"state":                   "UNKNOWN",
			"idleMinutes":             0,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	var out bytes.Buffer
	if err := runIdleStatus(idleStatusOptions{cred: cred, target: "909", out: &out}); err != nil {
		t.Fatalf("runIdleStatus error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "UNKNOWN") {
		t.Errorf("expected UNKNOWN to render honestly; got:\n%s", got)
	}
	if strings.Contains(got, "state       ACTIVE") || strings.Contains(got, "state       IDLE") {
		t.Errorf("UNKNOWN must never render as ACTIVE or IDLE; got:\n%s", got)
	}
}

func TestIdleStatusRequiresDeploymentID(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	err := idle([]string{"status"})
	if err == nil || !strings.Contains(err.Error(), "deployment id is required") {
		t.Fatalf("expected missing-id error, got: %v", err)
	}
}

func TestIdleUnknownSubcommand(t *testing.T) {
	err := idle([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("expected unknown-subcommand error, got: %v", err)
	}
}

// --- aq idle set: flag parsing / request-body construction ---

func TestParseIdleSetArgsOnlySendsSuppliedFields(t *testing.T) {
	opts, err := parseIdleSetArgs([]string{"4242", "--warn-after", "30m"})
	if err != nil {
		t.Fatalf("parseIdleSetArgs error: %v", err)
	}
	if opts.warnAfter == nil || *opts.warnAfter != 30 {
		t.Fatalf("expected warnAfter=30, got %v", opts.warnAfter)
	}
	if opts.stopAfter != nil {
		t.Errorf("stopAfter must stay nil when --stop-after was not passed, got %v", opts.stopAfter)
	}
	if opts.gpuThreshold != nil {
		t.Errorf("gpuThreshold must stay nil when --gpu-threshold was not passed, got %v", opts.gpuThreshold)
	}
	if opts.autoStopEnabled != nil {
		t.Errorf("autoStopEnabled must stay nil when neither --on nor --off was passed, got %v", opts.autoStopEnabled)
	}
	if opts.target != "4242" {
		t.Errorf("expected target 4242, got %q", opts.target)
	}
}

func TestParseIdleSetArgsAllFlags(t *testing.T) {
	opts, err := parseIdleSetArgs([]string{"--warn-after", "1h", "--stop-after", "2h", "--gpu-threshold", "10", "--on", "4242"})
	if err != nil {
		t.Fatalf("parseIdleSetArgs error: %v", err)
	}
	if opts.warnAfter == nil || *opts.warnAfter != 60 {
		t.Fatalf("expected warnAfter=60, got %v", opts.warnAfter)
	}
	if opts.stopAfter == nil || *opts.stopAfter != 120 {
		t.Fatalf("expected stopAfter=120, got %v", opts.stopAfter)
	}
	if opts.gpuThreshold == nil || *opts.gpuThreshold != 10 {
		t.Fatalf("expected gpuThreshold=10, got %v", opts.gpuThreshold)
	}
	if opts.autoStopEnabled == nil || *opts.autoStopEnabled != true {
		t.Fatalf("expected autoStopEnabled=true, got %v", opts.autoStopEnabled)
	}
}

func TestParseIdleSetArgsOffSetsFalse(t *testing.T) {
	opts, err := parseIdleSetArgs([]string{"4242", "--off"})
	if err != nil {
		t.Fatalf("parseIdleSetArgs error: %v", err)
	}
	if opts.autoStopEnabled == nil || *opts.autoStopEnabled != false {
		t.Fatalf("expected autoStopEnabled=false, got %v", opts.autoStopEnabled)
	}
}

func TestParseIdleSetArgsRejectsOnAndOff(t *testing.T) {
	_, err := parseIdleSetArgs([]string{"4242", "--on", "--off"})
	if err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Fatalf("expected --on/--off conflict error, got: %v", err)
	}
}

// TestParseIdleSetArgsRejectsWarnGEStop is the client-side mirror of the
// server's `warnAfterMinutes < actAfterMinutes` rule — it must fail before any
// round trip.
func TestParseIdleSetArgsRejectsWarnGEStop(t *testing.T) {
	_, err := parseIdleSetArgs([]string{"4242", "--warn-after", "1h", "--stop-after", "1h"})
	if err == nil || !strings.Contains(err.Error(), "must be less than") {
		t.Fatalf("expected warn<stop validation error, got: %v", err)
	}

	_, err = parseIdleSetArgs([]string{"4242", "--warn-after", "2h", "--stop-after", "1h"})
	if err == nil || !strings.Contains(err.Error(), "must be less than") {
		t.Fatalf("expected warn<stop validation error, got: %v", err)
	}
}

func TestParseIdleSetArgsRejectsBadGPUThreshold(t *testing.T) {
	_, err := parseIdleSetArgs([]string{"4242", "--gpu-threshold", "150"})
	if err == nil || !strings.Contains(err.Error(), "0-100") {
		t.Fatalf("expected gpu-threshold range error, got: %v", err)
	}
}

func TestParseIdleSetArgsRejectsBadDuration(t *testing.T) {
	_, err := parseIdleSetArgs([]string{"4242", "--warn-after", "not-a-duration"})
	if err == nil || !strings.Contains(err.Error(), "invalid --warn-after") {
		t.Fatalf("expected duration parse error, got: %v", err)
	}
}

func TestParseIdleSetArgsRejectsNoFlags(t *testing.T) {
	_, err := parseIdleSetArgs([]string{"4242"})
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Fatalf("expected nothing-to-update error, got: %v", err)
	}
}

func TestRunIdleSetSendsOnlySuppliedFieldsOverTheWire(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	var gotBody map[string]any
	mux.HandleFunc("/deployments/4242/idle-policy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		// PUT /idle-policy's real response carries no "state"/"idleMinutes" —
		// the orchestrator only computes a live verdict on GET. Omitting them
		// here matches production and is what TestRunIdleSetOmitsStateLine pins.
		writeData(w, map[string]any{
			"warnAfterMinutes":        30,
			"actAfterMinutes":         60,
			"gpuIdleThresholdPercent": 5,
			"autoStopEnabled":         true,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	warn := 30
	opts := idleSetOptions{cred: cred, target: "4242", warnAfter: &warn, out: &bytes.Buffer{}}
	if err := runIdleSet(opts); err != nil {
		t.Fatalf("runIdleSet error: %v", err)
	}

	if _, ok := gotBody["warnAfterMinutes"]; !ok {
		t.Errorf("expected warnAfterMinutes in request body; got: %v", gotBody)
	}
	for _, unset := range []string{"actAfterMinutes", "gpuIdleThresholdPercent", "autoStopEnabled"} {
		if _, ok := gotBody[unset]; ok {
			t.Errorf("did not expect %q in request body (flag was never passed); got: %v", unset, gotBody)
		}
	}
}

// TestRunIdleSetOmitsStateLine pins the fix for the empty "state" line: the
// PUT /idle-policy response has no state field (the orchestrator never
// computes a live verdict on write), so `aq idle set`'s output must not
// print a "state" line at all — printing one with nothing after it is worse
// than not printing it.
func TestRunIdleSetOmitsStateLine(t *testing.T) {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/deployments/4242/idle-policy", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"warnAfterMinutes":        30,
			"actAfterMinutes":         60,
			"gpuIdleThresholdPercent": 5,
			"autoStopEnabled":         true,
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cred := &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"}
	warn := 30
	var out bytes.Buffer
	opts := idleSetOptions{cred: cred, target: "4242", warnAfter: &warn, out: &out}
	if err := runIdleSet(opts); err != nil {
		t.Fatalf("runIdleSet error: %v", err)
	}

	got := out.String()
	if strings.Contains(got, "state") {
		t.Errorf("expected no state line in `aq idle set` output; got:\n%s", got)
	}
	for _, want := range []string{"30m", "1h", "enabled", "5%"} {
		if !strings.Contains(got, want) {
			t.Errorf("idle set output missing %q; got:\n%s", want, got)
		}
	}
}

// TestPrintIdlePolicySettingsOmitsState is a narrower unit test on the
// renderer itself: even if a caller passes a populated State field, the
// settings-only renderer used by `aq idle set` must never print it — only
// printIdlePolicy (used by `aq idle status`) renders live state.
func TestPrintIdlePolicySettingsOmitsState(t *testing.T) {
	var out bytes.Buffer
	printIdlePolicySettings(&out, api.IdlePolicy{
		WarnAfterMinutes:        30,
		ActAfterMinutes:         60,
		GPUIdleThresholdPercent: 5,
		AutoStopEnabled:         true,
		State:                   "ACTIVE",
		IdleMinutes:             0,
	})
	if strings.Contains(out.String(), "state") {
		t.Errorf("printIdlePolicySettings must never print a state line; got:\n%s", out.String())
	}
}

func TestFormatMinutes(t *testing.T) {
	cases := map[int]string{
		0:   "0m",
		5:   "5m",
		30:  "30m",
		60:  "1h",
		90:  "1h30m",
		120: "2h",
	}
	for in, want := range cases {
		if got := formatMinutes(in); got != want {
			t.Errorf("formatMinutes(%d) = %q, want %q", in, got, want)
		}
	}
}
