package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// candidatePubKeys lists the local public keys aq will offer to register, in
// preference order (modern first).
var candidatePubKeys = []string{
	"id_ed25519.pub",
	"id_rsa.pub",
	"id_ecdsa.pub",
}

// readLocalPublicKey returns the contents of the first existing public key under
// ~/.ssh, or an error describing how to create one. The returned string is a
// single-line OpenSSH public key suitable for POST /settings/ssh-keys.
func readLocalPublicKey() (path string, key string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve home directory: %w", err)
	}
	sshDir := filepath.Join(home, ".ssh")
	for _, name := range candidatePubKeys {
		p := filepath.Join(sshDir, name)
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			continue
		}
		k := strings.TrimSpace(string(data))
		if k != "" {
			return p, k, nil
		}
	}
	return "", "", fmt.Errorf(
		"no SSH public key found under %s (looked for %s)\n"+
			"Create one with:  ssh-keygen -t ed25519",
		sshDir, strings.Join(candidatePubKeys, ", "),
	)
}
