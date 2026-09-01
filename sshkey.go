package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// candidatePrivKeys lists the user's own private keys aq will adopt, in
// preference order (modern first). Adopting a key the user already has keeps
// their setup un-fragmented; aq only falls back to its own managed key when
// none of these is usable.
var candidatePrivKeys = []string{
	"id_ed25519",
	"id_rsa",
	"id_ecdsa",
}

// managedKeyName is the keypair aq generates and owns when the user has none.
//
// It is deliberately NOT id_ed25519: that path belongs to the user's own
// tooling, and creating it as a side effect of `aq up` would silently change
// what every other ssh invocation on the machine defaults to. A namespaced key
// can be rotated or deleted with zero blast radius. It is also deliberately
// under ~/.ssh rather than the aq config dir, which on macOS resolves to
// "~/Library/Application Support/aq" — a path with a space in it, which is a
// live quoting footgun inside an ssh_config IdentityFile directive.
const managedKeyName = "aquanode_ed25519"

// localKey is a usable local keypair: both halves present and readable.
type localKey struct {
	PrivatePath string
	PublicPath  string
	PublicKey   string // single-line OpenSSH public key, for POST /settings/ssh-keys
}

// sshDir returns the user's ~/.ssh, creating it 0700 if absent.
func sshDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve home directory: %w", err)
	}
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	// MkdirAll leaves an existing dir's mode untouched; ssh refuses to use a
	// group/world-writable ~/.ssh, so tighten it explicitly.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("tighten %s perms: %w", dir, err)
	}
	return dir, nil
}

// findLocalKey returns a usable local keypair without generating one, reporting
// ok=false when the machine has no key aq can use.
//
// Selection order: the AQ_SSH_KEY override, then the user's own candidate keys,
// then aq's managed key if it was generated on an earlier run.
//
// A candidate is only accepted when BOTH halves exist. Checking only for the
// .pub — as aq did before #422 — accepts an orphan public key (common after
// copying dotfiles between machines), provisions the box with a key the user
// holds no private half for, and surfaces minutes later as a bare
// "Permission denied (publickey)" with nothing pointing at the cause.
func findLocalKey() (localKey, bool, error) {
	if override := strings.TrimSpace(os.Getenv("AQ_SSH_KEY")); override != "" {
		k, err := loadKeyPair(override)
		if err != nil {
			return localKey{}, false, fmt.Errorf("AQ_SSH_KEY=%s is not a usable keypair: %w", override, err)
		}
		return k, true, nil
	}

	dir, err := sshDir()
	if err != nil {
		return localKey{}, false, err
	}
	for _, name := range append(append([]string{}, candidatePrivKeys...), managedKeyName) {
		if k, err := loadKeyPair(filepath.Join(dir, name)); err == nil {
			return k, true, nil
		}
	}
	return localKey{}, false, nil
}

// resolveLocalKey returns a usable local keypair, generating aq's managed key
// when the machine has none. `aq up` must never dead-end a user over a missing
// key — telling them to go run ssh-keygen themselves breaks the one-command
// promise for exactly the users who have never used ssh before.
func resolveLocalKey() (localKey, error) {
	k, ok, err := findLocalKey()
	if err != nil {
		return localKey{}, err
	}
	if ok {
		return k, nil
	}

	dir, err := sshDir()
	if err != nil {
		return localKey{}, err
	}
	path := filepath.Join(dir, managedKeyName)
	if err := generateManagedKey(path); err != nil {
		return localKey{}, err
	}
	return loadKeyPair(path)
}

// loadKeyPair reads the keypair at privPath (+ ".pub"), requiring both halves.
func loadKeyPair(privPath string) (localKey, error) {
	pubPath := privPath + ".pub"
	if _, err := os.Stat(privPath); err != nil {
		return localKey{}, fmt.Errorf("no private key at %s: %w", privPath, err)
	}
	data, err := os.ReadFile(pubPath)
	if err != nil {
		return localKey{}, fmt.Errorf("no public key at %s: %w", pubPath, err)
	}
	pub := strings.TrimSpace(string(data))
	if pub == "" {
		return localKey{}, fmt.Errorf("public key %s is empty", pubPath)
	}
	return localKey{PrivatePath: privPath, PublicPath: pubPath, PublicKey: pub}, nil
}

// generateManagedKey writes a fresh passphrase-less ed25519 keypair at path by
// shelling out to ssh-keygen.
//
// Shelling out rather than generating in-process is deliberate: aq's go.mod has
// zero dependencies, and stdlib crypto/ed25519 produces the key but not the
// OpenSSH on-disk format — serialising that correctly needs golang.org/x/crypto.
// ssh-keygen also ships in the same OpenSSH package as ssh itself, which `aq
// ssh` hard-requires anyway, so it is never the missing piece in practice.
//
// No passphrase: an interactive passphrase prompt breaks the one-command
// promise and aq has no agent-loading logic. This is the same at-rest posture as
// the API token already stored in credentials.json.
func generateManagedKey(path string) error {
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return fmt.Errorf("no SSH key found and ssh-keygen is not installed. Install OpenSSH, or create a key with: ssh-keygen -t ed25519")
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "laptop"
	}
	cmd := exec.Command(bin, "-t", "ed25519", "-f", path, "-N", "", "-C", "aq@"+host, "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("could not generate an SSH key at %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	// ssh-keygen writes 0600/0644 itself, but chmod explicitly in case the paths
	// already existed with looser modes.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("tighten %s perms: %w", path, err)
	}
	if err := os.Chmod(path+".pub", 0o644); err != nil {
		return fmt.Errorf("tighten %s perms: %w", path+".pub", err)
	}
	return nil
}
