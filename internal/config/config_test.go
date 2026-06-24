package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// TestSaveTightensLooseExistingFile ensures Save repairs a credentials file that
// already exists with world-readable perms — WriteFile alone won't chmod it.
func TestSaveTightensLooseExistingFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AQ_CONFIG_DIR", dir)

	path := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(path, []byte(`{"token":"old"}`), 0o644); err != nil {
		t.Fatalf("seed loose file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod loose file: %v", err)
	}

	if err := Save(&Credential{Token: "new"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("loose file not tightened: perm = %o, want 600", perm)
	}
}

// TestSaveTightensLooseExistingDir ensures Save repairs a config dir that already
// exists with group/world perms — MkdirAll leaves an existing dir's mode alone.
func TestSaveTightensLooseExistingDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "aq")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seed loose dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod loose dir: %v", err)
	}
	t.Setenv("AQ_CONFIG_DIR", dir)

	if err := Save(&Credential{Token: "new"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("loose dir not tightened: perm = %o, want 700", perm)
	}
}

// TestSaveWarnsOnExposedParent ensures Save surfaces a warning when the config
// dir's parent is group/world-accessible.
func TestSaveWarnsOnExposedParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	dir := filepath.Join(parent, "aq")
	t.Setenv("AQ_CONFIG_DIR", dir)

	var buf bytes.Buffer
	orig := warnOut
	warnOut = &buf
	t.Cleanup(func() { warnOut = orig })

	if err := Save(&Credential{Token: "new"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !strings.Contains(buf.String(), "accessible to other users") {
		t.Errorf("expected exposed-parent warning, got %q", buf.String())
	}
}

// TestSaveQuietOnPrivateParent ensures Save does not warn when the parent is 0700.
func TestSaveQuietOnPrivateParent(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	dir := filepath.Join(parent, "aq")
	t.Setenv("AQ_CONFIG_DIR", dir)

	var buf bytes.Buffer
	orig := warnOut
	warnOut = &buf
	t.Cleanup(func() { warnOut = orig })

	if err := Save(&Credential{Token: "new"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected warning on private parent: %q", buf.String())
	}
}
