package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// knownHostsName is the known_hosts file aq owns, wired into every generated
// stanza via UserKnownHostsFile.
//
// A dedicated file, rather than the user's ~/.ssh/known_hosts, is doing real
// work here: GPU providers recycle public IPs aggressively, so an address aq
// recorded will eventually be handed to an unrelated host the user actually
// cares about. Writing into their real known_hosts would then greet them with
// the full-screen REMOTE HOST IDENTIFICATION HAS CHANGED banner against their
// own infrastructure. Isolation keeps that blast radius inside aq's fleet.
const knownHostsName = "aquanode_known_hosts"

func knownHostsPath() (string, error) {
	dir, err := sshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, knownHostsName), nil
}

// hostKeyLine renders a known_hosts entry. A non-default port MUST use the
// bracket form — the classic silent bug, and four of the ten providers
// (vastai, simplepod's docker pool, voltagepark, akash) publish sshd on a
// dynamic non-22 port.
func hostKeyLine(host, port, key string) string {
	return hostPattern(host, port) + " " + key
}

// hostPattern renders the host[:port] pattern a known_hosts line is keyed by.
func hostPattern(host, port string) string {
	if port == "" || port == strconv.Itoa(api.SSHPort) {
		return host
	}
	return "[" + host + "]:" + port
}

// seedHostKey records a provider-reported host key so the first connection is
// authenticated rather than trust-on-first-use.
//
// It is a deliberate no-op today: no host key exists anywhere in the platform —
// not in the Prisma schema, not on any API response. (The `fingerprint` and
// `ssh_key` fields nearby are the user's *client* key, not the box's.) Plumbing
// a real one is a mjolnir → orchestrator → schema → API change across repos.
// Until then the generated stanzas rely on StrictHostKeyChecking=accept-new,
// which is also what mjolnir itself uses internally. This function exists so
// that field can light up with no restructuring on the CLI side.
func seedHostKey(host, port, hostKey string) error {
	if strings.TrimSpace(hostKey) == "" {
		return nil
	}
	path, err := knownHostsPath()
	if err != nil {
		return err
	}
	kept, err := readKnownHostsExcept(path, host, port)
	if err != nil {
		return err
	}
	kept = append(kept, hostKeyLine(host, port, strings.TrimSpace(hostKey)))
	return atomicWrite(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

// removeHost drops every entry for a host[:port] from aq's known_hosts.
//
// `aq down` calls this so a later deployment landing on the same recycled
// address is not rejected as a host-key mismatch — the single most likely way
// this feature would start annoying a heavy user in month two.
func removeHost(host, port string) error {
	path, err := knownHostsPath()
	if err != nil {
		return err
	}
	kept, err := readKnownHostsExcept(path, host, port)
	if err != nil {
		return err
	}
	if len(kept) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	return atomicWrite(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

// readKnownHostsExcept returns aq's known_hosts lines minus those keyed by the
// given host[:port]. A missing file yields no lines rather than an error.
func readKnownHostsExcept(path, host, port string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	want := hostPattern(host, port)
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == want {
			continue
		}
		kept = append(kept, line)
	}
	return kept, nil
}
