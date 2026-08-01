package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
)

// Markers delimiting the three lines aq owns inside the user's own ~/.ssh/config.
const (
	beginMarker = "# BEGIN aquanode"
	endMarker   = "# END aquanode"
)

// managedConfigName is the ssh_config fragment aq owns outright and regenerates
// wholesale. The user's own config only ever gains an Include pointing here.
const managedConfigName = "aquanode.config"

// sshUser is the login on every Aquanode box. There is no per-deployment user
// field anywhere in the platform, and none is needed: ogre installs the user's
// public key into /root/.ssh/authorized_keys on every provider, so the end user
// lands on root regardless of which provider mjolnir used to reach the box.
const sshUser = "root"

// sshEntry is one generated Host stanza.
type sshEntry struct {
	Alias        string
	HostName     string
	Port         string
	IdentityFile string
	KnownHosts   string
}

func managedConfigPath() (string, error) {
	dir, err := sshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, managedConfigName), nil
}

func userConfigPath() (string, error) {
	dir, err := sshDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config"), nil
}

// aliasFor derives the ssh alias for a deployment: `aq-<slug of name>`, falling
// back to `aq-<deploymentId>` when the name is absent or slugs to nothing.
func aliasFor(name string, deploymentID int) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		return "aq-" + strconv.Itoa(deploymentID)
	}
	return "aq-" + slug
}

// entriesFor builds the Host stanzas for one deployment: the ergonomic
// name-derived alias plus the always-stable `aq-<id>` alias, so a box is
// reachable both ways and an unnamed box still gets a handle.
func entriesFor(dep api.Deployment, identityFile, knownHosts string) []sshEntry {
	host, port, ok := sshEndpointFor(dep)
	if !ok {
		return nil
	}
	if port == "" {
		port = strconv.Itoa(api.SSHPort)
	}

	seen := map[string]bool{}
	var entries []sshEntry
	for _, alias := range []string{aliasFor(dep.Name, dep.ID), "aq-" + strconv.Itoa(dep.ID)} {
		if seen[alias] {
			continue
		}
		seen[alias] = true
		entries = append(entries, sshEntry{
			Alias:        alias,
			HostName:     host,
			Port:         port,
			IdentityFile: identityFile,
			KnownHosts:   knownHosts,
		})
	}
	return entries
}

// renderManagedConfig renders the whole fragment aq owns.
//
// Paths are written absolute rather than as `~/...`: OpenSSH expands a tilde
// from the passwd database (getpwuid), while aq resolves its own paths from
// $HOME. Those agree for a normal login and disagree inside a container or CI
// job that sets HOME elsewhere — at which point a tilde'd IdentityFile points
// at a key that isn't there. An absolute path has no expansion step to disagree
// about.
func renderManagedConfig(entries []sshEntry) string {
	var b strings.Builder
	b.WriteString("# Managed by `aq` — regenerated on every aq ssh / up / down.\n")
	b.WriteString("# Do not edit: your changes will be overwritten.\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "\nHost %s\n", e.Alias)
		fmt.Fprintf(&b, "    HostName %s\n", e.HostName)
		fmt.Fprintf(&b, "    Port %s\n", e.Port)
		fmt.Fprintf(&b, "    User %s\n", sshUser)
		fmt.Fprintf(&b, "    IdentityFile %s\n", e.IdentityFile)
		b.WriteString("    IdentitiesOnly yes\n")
		fmt.Fprintf(&b, "    UserKnownHostsFile %s\n", e.KnownHosts)
		b.WriteString("    StrictHostKeyChecking accept-new\n")
	}
	return b.String()
}

// applyIncludeRegion returns the user's ssh_config with aq's Include region
// present exactly once, at the top.
//
// The region must be at the TOP, and aq's stanzas must live behind an Include
// rather than being appended to the user's file, because ssh_config is
// FIRST-match-wins per keyword — not last-match-wins. A user with an earlier
// `Host *` block setting IdentityFile / User / ProxyCommand (corporate
// ProxyJump, a 1Password or Secretive agent) would silently win over stanzas
// appended at EOF, and aq's directives would be partially ignored with no error
// anywhere. Appending is the intuitive move and it is wrong.
//
// Everything outside the markers is preserved byte for byte.
func applyIncludeRegion(existing, includePath string) (string, error) {
	region := beginMarker + " — managed by `aq`, do not edit this block\n" +
		"Include " + includePath + "\n" +
		endMarker + "\n"

	lines := strings.Split(existing, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, beginMarker):
			if begin >= 0 {
				return "", fmt.Errorf("found a second %q marker at line %d — remove the stray marker and re-run", beginMarker, i+1)
			}
			begin = i
		case strings.HasPrefix(trimmed, endMarker):
			if end >= 0 {
				return "", fmt.Errorf("found a second %q marker at line %d — remove the stray marker and re-run", endMarker, i+1)
			}
			end = i
		}
	}

	switch {
	case begin < 0 && end < 0:
		if strings.TrimSpace(existing) == "" {
			return region, nil
		}
		return region + "\n" + existing, nil
	case begin < 0 || end < 0:
		// Never guess at repairing a half-marked config — an ssh_config aq
		// mangles can lock the user out of every host they use.
		return "", fmt.Errorf("found %q without its matching partner — remove the stray marker and re-run", map[bool]string{true: endMarker, false: beginMarker}[begin < 0])
	case end < begin:
		return "", fmt.Errorf("found %q before %q — remove the stray markers and re-run", endMarker, beginMarker)
	}

	rebuilt := append([]string{}, lines[:begin]...)
	rebuilt = append(rebuilt, strings.TrimSuffix(region, "\n"))
	rebuilt = append(rebuilt, lines[end+1:]...)
	return strings.Join(rebuilt, "\n"), nil
}

// ensureIncludeRegion makes sure the user's ~/.ssh/config includes aq's
// fragment. It is a no-op once the region is present and correct.
func ensureIncludeRegion() error {
	path, err := userConfigPath()
	if err != nil {
		return err
	}
	includePath, err := managedConfigPath()
	if err != nil {
		return err
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	updated, err := applyIncludeRegion(string(existing), includePath)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if updated == string(existing) {
		return nil
	}
	return atomicWrite(path, []byte(updated), 0o600)
}

// writeManagedConfig regenerates aq's fragment wholesale.
//
// Regenerating beats upserting individual stanzas: it self-heals a fragment
// that drifted from server truth (a box terminated from the console, an IP that
// changed across a restart, a stanza hand-deleted) instead of accumulating dead
// aliases that fail later as a confusing timeout.
func writeManagedConfig(entries []sshEntry) error {
	path, err := managedConfigPath()
	if err != nil {
		return err
	}
	data := []byte(renderManagedConfig(entries))
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}
	return atomicWrite(path, data, 0o600)
}

// syncManagedConfig rewrites aq's ssh_config fragment from the team's live
// deployments. extra rows (a just-provisioned box whose fresher row the caller
// already holds) override their list counterpart; excludeID drops a deployment
// aq just closed, whose row the list still reports as live for a while because
// teardown is asynchronous.
func syncManagedConfig(client *api.Client, extra []api.Deployment, excludeID int) error {
	identityFile, knownHosts, err := sshPaths()
	if err != nil {
		return err
	}

	deps, err := client.ListDeployments()
	if err != nil {
		return fmt.Errorf("could not list deployments: %w", err)
	}

	live := map[int]api.Deployment{}
	for _, d := range deps {
		if d.ID > 0 && !isClosedStatus(d.Status) {
			live[d.ID] = d
		}
	}
	for _, d := range extra {
		if d.ID > 0 {
			live[d.ID] = d
		}
	}
	delete(live, excludeID)

	ids := make([]int, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))

	var entries []sshEntry
	for _, id := range ids {
		entries = append(entries, entriesFor(live[id], identityFile, knownHosts)...)
	}

	if err := ensureIncludeRegion(); err != nil {
		return err
	}
	return writeManagedConfig(entries)
}

// syncManagedConfigQuiet syncs the ssh_config fragment on a best-effort basis.
// `aq up` / `aq status` must not fail over it: the box is rented and running,
// and reporting that as a failure because a dotfile write did not land would be
// a worse lie than a missing alias.
func syncManagedConfigQuiet(client *api.Client, errOut io.Writer, extra []api.Deployment, excludeID int) {
	if err := syncManagedConfig(client, extra, excludeID); err != nil {
		fmt.Fprintf(errOut, "warning: could not update your ssh config: %v\n", err)
	}
}

// sshPaths returns the IdentityFile and UserKnownHostsFile the generated
// stanzas should point at. When no local key exists yet it names the managed
// key path anyway — that is where the next `aq up` will generate one, and ssh
// reports a missing identity file far more clearly than a stanza with none.
func sshPaths() (identityFile, knownHosts string, err error) {
	dir, err := sshDir()
	if err != nil {
		return "", "", err
	}
	identityFile = filepath.Join(dir, managedKeyName)
	if k, ok, err := findLocalKey(); err == nil && ok {
		identityFile = k.PrivatePath
	}
	return identityFile, filepath.Join(dir, knownHostsName), nil
}

// atomicWrite replaces path via a same-directory temp file + rename.
//
// The temp file MUST share a directory with the target: os.Rename cannot cross
// filesystems. A half-written ~/.ssh/config would lock the user out of every
// host they use, so this is not optional tidiness.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".aq-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
