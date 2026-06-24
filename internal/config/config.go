// Package config handles the on-disk `aq` credential store and endpoint
// resolution. The stored CLI credential is a long-lived API token minted by the
// device-login (`aq login`) pairing flow; it lives in a 0600 file under the
// user's config dir and is never logged.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultAPIURL is the Aquanode API base used when AQ_API_URL is unset.
const DefaultAPIURL = "https://server.aquanode.io/api/v1"

// Credential is the persisted result of `aq login`.
type Credential struct {
	APIURL  string   `json:"apiUrl"`
	Token   string   `json:"token"`
	TeamID  string   `json:"teamId,omitempty"`
	KeyName string   `json:"keyName,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

// APIURL returns the configured Aquanode API base URL, honoring AQ_API_URL.
func APIURL() string {
	if v := os.Getenv("AQ_API_URL"); v != "" {
		return v
	}
	return DefaultAPIURL
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

// Save writes the credential to disk with 0600 perms (0700 dir).
func Save(cred *Credential) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
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
	return nil
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
