package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKeyPair(t *testing.T, dir, name, pub string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("PRIVATE\n"), 0o600); err != nil {
		t.Fatalf("write private: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".pub"), []byte(pub+"\n"), 0o644); err != nil {
		t.Fatalf("write public: %v", err)
	}
}

// TestResolveLocalKeyIgnoresOrphanPubKey is the #422 regression. aq used to
// accept a lone id_ed25519.pub — common after copying dotfiles between machines
// — and provision a box with a key the user holds no private half for. The
// failure then surfaced minutes later as a bare "Permission denied (publickey)"
// with nothing anywhere pointing at the cause.
func TestResolveLocalKeyIgnoresOrphanPubKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AQ_SSH_KEY", "")
	sshPath := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	orphan := filepath.Join(sshPath, "id_ed25519.pub")
	if err := os.WriteFile(orphan, []byte("ssh-ed25519 ORPHAN orphan@old-laptop\n"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	key, err := resolveLocalKey()
	if err != nil {
		t.Fatalf("resolveLocalKey: %v", err)
	}
	if strings.Contains(key.PublicKey, "ORPHAN") {
		t.Fatalf("adopted an orphan .pub with no private half: %s", key.PublicPath)
	}
	if filepath.Base(key.PrivatePath) != managedKeyName {
		t.Errorf("expected a fall-through to the managed key, got %s", key.PrivatePath)
	}
	if _, err := os.Stat(key.PrivatePath); err != nil {
		t.Errorf("managed private key was not written: %v", err)
	}
}

// TestResolveLocalKeyGeneratesWhenNoKeyExists covers the whole point of #422:
// a user with no key material at all must never be told to go run ssh-keygen.
func TestResolveLocalKeyGeneratesWhenNoKeyExists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AQ_SSH_KEY", "")

	key, err := resolveLocalKey()
	if err != nil {
		t.Fatalf("resolveLocalKey: %v", err)
	}
	if !strings.HasPrefix(key.PublicKey, "ssh-ed25519 ") {
		t.Errorf("expected a generated ed25519 key, got %q", key.PublicKey)
	}
	info, err := os.Stat(key.PrivatePath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key must be 0600, got %#o", perm)
	}

	// A second call reuses the generated key rather than churning a new one —
	// regenerating would orphan every box already provisioned with the old key.
	again, err := resolveLocalKey()
	if err != nil {
		t.Fatalf("second resolveLocalKey: %v", err)
	}
	if again.PublicKey != key.PublicKey {
		t.Error("managed key was regenerated instead of reused")
	}
}

// TestResolveLocalKeyPrefersUsersOwnKey keeps aq from fragmenting a setup that
// already works.
func TestResolveLocalKeyPrefersUsersOwnKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AQ_SSH_KEY", "")
	writeKeyPair(t, filepath.Join(home, ".ssh"), "id_ed25519", "ssh-ed25519 MINE me@laptop")

	key, err := resolveLocalKey()
	if err != nil {
		t.Fatalf("resolveLocalKey: %v", err)
	}
	if !strings.Contains(key.PublicKey, "MINE") {
		t.Errorf("expected the user's own key, got %q from %s", key.PublicKey, key.PrivatePath)
	}
}

func TestFindLocalKeyHonoursAQSSHKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeKeyPair(t, filepath.Join(home, ".ssh"), "id_ed25519", "ssh-ed25519 DEFAULT me@laptop")

	explicit := filepath.Join(t.TempDir(), "ci_key")
	writeKeyPair(t, filepath.Dir(explicit), "ci_key", "ssh-ed25519 EXPLICIT ci@runner")
	t.Setenv("AQ_SSH_KEY", explicit)

	key, ok, err := findLocalKey()
	if err != nil || !ok {
		t.Fatalf("findLocalKey: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(key.PublicKey, "EXPLICIT") {
		t.Errorf("AQ_SSH_KEY must win, got %q", key.PublicKey)
	}

	t.Setenv("AQ_SSH_KEY", filepath.Join(home, "nope"))
	if _, _, err := findLocalKey(); err == nil {
		t.Error("an unusable AQ_SSH_KEY must be an error, not a silent fallback")
	}
}

func TestFindLocalKeyReportsNoneWithoutGenerating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AQ_SSH_KEY", "")

	_, ok, err := findLocalKey()
	if err != nil {
		t.Fatalf("findLocalKey: %v", err)
	}
	if ok {
		t.Error("expected no key on a clean home")
	}
	// `aq ssh` uses findLocalKey precisely so it never mints a key that the
	// already-running box could not possibly accept.
	if _, err := os.Stat(filepath.Join(home, ".ssh", managedKeyName)); !os.IsNotExist(err) {
		t.Error("findLocalKey must not generate a key")
	}
}
