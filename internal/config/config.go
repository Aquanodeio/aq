// Package config handles the on-disk `aq` credential store and endpoint
// resolution. The stored CLI credential is a long-lived API token minted by the
// device-login (`aq login`) pairing flow; it lives in a 0600 file under the
// user's config dir and is never logged.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// warnOut receives non-fatal permission warnings from Save. It defaults to
// stderr; tests override it to assert on the message.
var warnOut io.Writer = os.Stderr

// DefaultAPIURL is the Aquanode API base used when AQ_API_URL is unset.
const DefaultAPIURL = "https://server.aquanode.io/api/v1"

// DefaultConsoleURL is the public console host `aq share` builds a link
// against. Deliberately a separate constant from DefaultAPIURL — the API
// (server.aquanode.io) and the console app (console.aquanode.io) are
// different domains, and a share link points at the latter.
const DefaultConsoleURL = "https://console.aquanode.io"

// Credential is the persisted result of `aq login`.
type Credential struct {
	APIURL  string   `json:"apiUrl"`
	Token   string   `json:"token"`
	TeamID  string   `json:"teamId,omitempty"`
	KeyName string   `json:"keyName,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

// APIURLOverride returns AQ_API_URL when it is set, else "".
//
// Split out from APIURL so callers can tell "the operator explicitly pointed
// this run somewhere" from "nothing was configured, use the default". That
// distinction is the whole reason the override works at all — see
// resolveAPIURL.
func APIURLOverride() string {
	return os.Getenv("AQ_API_URL")
}

// APIURL returns the configured Aquanode API base URL, honoring AQ_API_URL.
func APIURL() string {
	if v := APIURLOverride(); v != "" {
		return v
	}
	return DefaultAPIURL
}

// ConsoleURL returns the configured Aquanode console base URL, honoring
// AQ_CONSOLE_URL (mirrors APIURL/AQ_API_URL, for pointing at a non-prod
// console during local dev).
func ConsoleURL() string {
	if v := os.Getenv("AQ_CONSOLE_URL"); v != "" {
		return v
	}
	return DefaultConsoleURL
}

// Dir returns the directory holding aq's config, honoring AQ_CONFIG_DIR then
// the OS user-config dir, falling back to ~/.config/aq.
func Dir() (string, error) {
	if v := os.Getenv("AQ_CONFIG_DIR"); v != "" {
		return v, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", fmt.Errorf("cannot resolve a config directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "aq"), nil
}

func credentialPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// Save writes the credential to disk with 0600 perms (0700 dir). If the config
// dir or credential file already exist with looser permissions, Save repairs
// them so a token written by an older aq (or another tool) can't linger as
// group/world-readable.
func Save(cred *Credential) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// MkdirAll leaves an already-existing dir's mode untouched, so tighten it
	// explicitly in case it was created loosely by an older aq or another tool.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("tighten config dir perms: %w", err)
	}
	path := filepath.Join(dir, "credentials.json")
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	// Write 0600 so the token is never world-readable.
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	// WriteFile only applies its mode when creating the file; an existing file
	// keeps its old (possibly loose) perms, so chmod it explicitly.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("tighten credentials perms: %w", err)
	}
	warnIfParentExposed(dir)
	return nil
}

// warnIfParentExposed prints a one-line warning when the config dir's parent is
// group- or world-accessible. The dir itself is 0700 so the token stays safe,
// but a loose parent is worth surfacing to the user.
func warnIfParentExposed(dir string) {
	parent := filepath.Dir(dir)
	info, err := os.Stat(parent)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		fmt.Fprintf(warnOut, "warning: %s is accessible to other users (mode %#o); your aq credentials dir is 0700, but consider tightening its parent.\n", parent, perm)
	}
}

// Load reads the stored credential, or returns (nil, nil) if none exists.
func Load() (*Credential, error) {
	path, err := credentialPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cred Credential
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return &cred, nil
}

// Clear removes the stored credential (used by `aq logout`). Returns whether a
// credential file existed.
func Clear() (bool, error) {
	path, err := credentialPath()
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
