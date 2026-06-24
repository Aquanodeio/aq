package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAPIURLDefaultAndOverride(t *testing.T) {
	t.Setenv("AQ_API_URL", "")
	if got := APIURL(); got != DefaultAPIURL {
		t.Errorf("default APIURL = %q, want %q", got, DefaultAPIURL)
	}
	t.Setenv("AQ_API_URL", "http://localhost:8080/api/v1")
	if got := APIURL(); got != "http://localhost:8080/api/v1" {
		t.Errorf("override APIURL = %q", got)
	}
}

func TestSaveLoadClearRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AQ_CONFIG_DIR", dir)

	// No credential yet.
	if cred, err := Load(); err != nil || cred != nil {
		t.Fatalf("expected nil credential, got %+v err %v", cred, err)
	}

	want := &Credential{
		APIURL:  "http://localhost:8080/api/v1",
		Token:   "aq_sk_secret",
		TeamID:  "team-1",
		KeyName: "aq CLI · box",
		Scopes:  []string{"full"},
	}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File must be 0600 so the token is not world-readable.
	info, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential file perm = %o, want 600", perm)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Token != want.Token || got.TeamID != want.TeamID || got.KeyName != want.KeyName {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	existed, err := Clear()
	if err != nil || !existed {
		t.Fatalf("Clear: existed=%v err=%v", existed, err)
	}
	if cred, _ := Load(); cred != nil {
		t.Errorf("credential still present after Clear: %+v", cred)
	}
	// Clearing again is a no-op.
	if existed, _ := Clear(); existed {
		t.Errorf("second Clear reported a file existed")
	}
}
