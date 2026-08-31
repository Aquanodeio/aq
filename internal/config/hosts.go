package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hostsFileName is the detached-mode host registry, kept beside credentials.json
// in the same 0700 config dir and written 0600 by the same helper that tightens
// the credential file.
//
// It lives here rather than in a store of its own because config already owns
// directory creation, permission repair, and the AQ_CONFIG_DIR override — a
// second store would have to re-derive all three and would drift from them the
// first time one changed. The registry holds no secret (an ssh target and a path
// to a key, never the key), but it is 0600 anyway: the list of machines a user
// runs GPU work on is not something to leave world-readable.
const hostsFileName = "hosts.json"

// Host is one detached-mode box: a machine the user owns or leases, that
// Aquanode never provisioned and — unless it is also attached — never talks to.
type Host struct {
	// Alias is the local handle. Box-facing verbs address it as `host:<alias>`.
	Alias string `json:"alias"`
	// SSH is the login target as the user typed it: `user@host`.
	SSH string `json:"ssh"`
	// Port is the box's sshd port; 0 means the ssh default (22).
	Port int `json:"port,omitempty"`
	// Identity is an ABSOLUTE path to the private key. Never `~`-relative:
	// OpenSSH expands a tilde from the passwd entry while aq resolves paths
	// from $HOME, and the two disagree inside a container or CI job.
	Identity string `json:"identity,omitempty"`
	// MountPath is the box's workspace root — where push/run land and what a
	// capture treats as the portable root. Empty means the aq default.
	MountPath string `json:"mountPath,omitempty"`
	// OgrePort is the port ogre's control API listens on once attached. It is
	// meaningless in pure detached mode, where the CLI reaches its daemon over
	// loopback and nothing dials the box from outside.
	OgrePort int `json:"ogrePort,omitempty"`
	// AddedAt is RFC3339, for `aq host ls`.
	AddedAt string `json:"addedAt,omitempty"`

	// DeploymentID is set only once `aq attach` has succeeded — the row the
	// orchestrator created for this box. Zero means detached-only, which is the
	// normal state and not a degraded one.
	DeploymentID int `json:"deploymentId,omitempty"`
	// PublicHost is the address the orchestrator dials for an attached box. It
	// is recorded separately from SSH because the two are not always the same
	// string (an ssh alias, a jump-host'd name), and the probe result is only
	// ever about this one.
	PublicHost string `json:"publicHost,omitempty"`
	// AttachedAt is RFC3339 and is stamped only after a successful server-side
	// reachability probe. A box is never recorded as attached on the strength
	// of a request that was merely accepted.
	AttachedAt string `json:"attachedAt,omitempty"`

	// PendingDeploymentID is set the moment `aq attach` registers the box with
	// the orchestrator (AdoptExternal), before the reachability probe that
	// gates DeploymentID/AttachedAt below. If attach fails anywhere after that
	// point — redeem, box configuration, or the probe itself — the orchestrator
	// already holds a PROVISIONING row for this box, and this field is the only
	// local record of it. It exists so `aq release` has something to act on
	// even when Attached() is false; a refused attach's own "release it with
	// `aq release <alias>`" was otherwise unfollowable. Cleared
	// to 0 the moment attach actually succeeds, and cleared by `aq release`
	// once the row is gone.
	PendingDeploymentID int `json:"pendingDeploymentId,omitempty"`
}

// Attached reports whether this host has a live control-plane row.
func (h Host) Attached() bool { return h.DeploymentID != 0 && h.AttachedAt != "" }

// Releasable reports whether `aq release` has anything to act on: either a
// fully attached row, or a row `aq attach` registered server-side but never
// finished activating.
func (h Host) Releasable() bool { return h.Attached() || h.PendingDeploymentID != 0 }

// ReleaseTargetID is the deployment id `aq release` should call
// ReleaseExternal on. Attached() wins when both are somehow set — it is the
// row that is actually live — but ordinarily only one of the two is nonzero.
func (h Host) ReleaseTargetID() int {
	if h.Attached() {
		return h.DeploymentID
	}
	return h.PendingDeploymentID
}

// hostsFile is the on-disk envelope. An object rather than a bare array so the
// format can grow a field without every older aq failing to parse it.
type hostsFile struct {
	Hosts []Host `json:"hosts"`
}

// HostsPath returns the registry's path.
func HostsPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hostsFileName), nil
}

// LoadHosts reads the registry, sorted by alias. A missing file is an empty
// registry, not an error — `aq host ls` on a fresh machine prints nothing and
// exits zero.
func LoadHosts() ([]Host, error) {
	path, err := HostsPath()
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
	var file hostsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	sort.Slice(file.Hosts, func(i, j int) bool { return file.Hosts[i].Alias < file.Hosts[j].Alias })
	return file.Hosts, nil
}

// FindHost returns the host registered under alias, or (Host{}, false).
func FindHost(alias string) (Host, bool, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return Host{}, false, err
	}
	for _, h := range hosts {
		if strings.EqualFold(h.Alias, alias) {
			return h, true, nil
		}
	}
	return Host{}, false, nil
}

// SaveHosts writes the whole registry with 0600 perms (0700 dir), reusing the
// same directory creation and permission repair Save applies to the credential.
func SaveHosts(hosts []Host) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	// MkdirAll leaves an existing dir's mode alone; tighten it explicitly, the
	// same way Save does for a dir an older aq may have created loosely.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("tighten config dir perms: %w", err)
	}
	if hosts == nil {
		hosts = []Host{}
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Alias < hosts[j].Alias })
	data, err := json.MarshalIndent(hostsFile{Hosts: hosts}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, hostsFileName)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write hosts: %w", err)
	}
	// WriteFile only applies its mode when it creates the file, so an existing
	// registry keeps whatever perms it had until this chmod.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("tighten hosts perms: %w", err)
	}
	return nil
}

// PutHost inserts or replaces one entry, leaving the rest untouched.
func PutHost(h Host) error {
	hosts, err := LoadHosts()
	if err != nil {
		return err
	}
	replaced := false
	for i := range hosts {
		if strings.EqualFold(hosts[i].Alias, h.Alias) {
			hosts[i] = h
			replaced = true
			break
		}
	}
	if !replaced {
		hosts = append(hosts, h)
	}
	return SaveHosts(hosts)
}

// RemoveHost drops one entry, reporting whether it was there.
func RemoveHost(alias string) (bool, error) {
	hosts, err := LoadHosts()
	if err != nil {
		return false, err
	}
	kept := make([]Host, 0, len(hosts))
	found := false
	for _, h := range hosts {
		if strings.EqualFold(h.Alias, alias) {
			found = true
			continue
		}
		kept = append(kept, h)
	}
	if !found {
		return false, nil
	}
	return true, SaveHosts(kept)
}
