package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// snapshotArgs holds `aq save`'s raw parsed CLI input, before target is
// resolved to a setup id.
type snapshotArgs struct {
	target  string // setup id (uuid) or name
	name    string // explicit --name override for the save lineage
	pathDir string
}

// snapshotOptions configures runSnapshot, once target has been resolved to a
// setup id. snapshot() fills in the real environment; tests call
// parseSnapshotArgs / runSnapshot directly.
type snapshotOptions struct {
	cred    *config.Credential
	setupID string // resolved setup id (uuid) — never a deployment id
	name    string
	pathDir string
}

// parseSnapshotArgs parses `aq save`'s flags and positional target, letting
// them appear in any order. Go's stdlib flag package stops parsing at the first
// positional, so `aq save comfyui --name x` would otherwise silently drop the
// flag; parseInterspersed is the shared workaround every verb here uses.
func parseSnapshotArgs(args []string) (snapshotArgs, error) {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	name := fs.String("name", "", "name for this setup's save lineage (default: the setup's own name; only matters on the first save)")
	path := fs.String("path", "/workspace", "directory to capture")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return snapshotArgs{}, err
	}
	opts := snapshotArgs{name: *name, pathDir: *path}
	if len(rest) > 0 {
		opts.target = rest[0]
	}
	return opts, nil
}

// snapshot parses flags and wires the real environment into runSnapshot,
// backing `aq save`.
//
// `aq save <setup>` saves a setup's current state on demand into its named
// lineage — the same one-shot capture the console's Save action drives,
// reachable without leaving the terminal.
func snapshot(args []string) error {
	parsed, err := parseSnapshotArgs(args)
	if err != nil {
		return err
	}
	if parsed.target == "" {
		return fmt.Errorf("a setup is required — usage: aq save <setup>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	client := newControlClient(cred)
	setupID, err := resolveSetupID(client, parsed.target)
	if err != nil {
		return err
	}

	res, err := runSnapshot(snapshotOptions{
		cred:    cred,
		setupID: setupID,
		name:    parsed.name,
		pathDir: parsed.pathDir,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "✓ Saved %s v%d\n", res.Name, res.Version)
	return nil
}

// runSnapshot saves a setup's current state into its named lineage, given an
// already-resolved setup id. Reused by `aq down --save` as the
// checkpoint step before terminating.
func runSnapshot(opts snapshotOptions) (api.SetupVersion, error) {
	client := newControlClient(opts.cred)

	name := opts.name
	if name == "" {
		name = lineageNameForFirstSave(client, opts.setupID)
	}

	res, err := client.CreateSetupSnapshot(opts.setupID, api.CreateSetupSnapshotRequest{
		Name:         name,
		WorkspaceDir: opts.pathDir,
	})
	if err != nil {
		return api.SetupVersion{}, err
	}
	if name != "" {
		markLineageNamed(opts.setupID)
	}
	return *res, nil
}

// isInteractiveStdin reports whether stdin is an interactive terminal.
// Overridable by tests. In a non-interactive context (piped/redirected
// stdin, or a CI job) the first-save name prompt must never block, so the
// default is used silently instead.
var isInteractiveStdin = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// lineageNameForFirstSave returns the name to send with a save, or "" once
// the setup already has a named lineage — later saves must omit the name so
// the server keeps reusing the existing one rather than treating a resent
// value as anything meaningful. On a setup's genuinely first save (per the
// local record in namedLineagesPath) it asks once: a single Enter accepts the
// setup's own name as the default, any other stdin is used verbatim, and a
// non-interactive stdin skips the prompt and uses the default silently. A
// lookup or prompt failure never blocks the save — it just falls back to the
// default name.
func lineageNameForFirstSave(client *api.Client, setupID string) string {
	if loadNamedLineages()[setupID] {
		return ""
	}

	defaultName := setupDisplayName(client, setupID)

	if !isInteractiveStdin() {
		return defaultName
	}

	fmt.Fprintf(os.Stdout, "Name this setup's save lineage [%s]: ", defaultName)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultName
	}
	return line
}

// setupDisplayName fetches a setup's own name (via GET /setups — there is no
// single-setup endpoint) to default its save-lineage name to. A failed
// lookup must never abort the save — it falls back to a generic label
// instead.
func setupDisplayName(client *api.Client, setupID string) string {
	setup, err := findSetup(client, setupID)
	if err != nil || setup.Name == "" {
		return fmt.Sprintf("setup-%s", setupID)
	}
	return setup.Name
}

// namedLineagesPath is aq's local record of which setups it has already
// asked (or defaulted) a save-lineage name for, so `aq save` only prompts
// once per setup even across separate CLI invocations. This is a
// best-effort client-side cache, not the source of truth — the server is
// free to already have a lineage bound even if this file doesn't know it
// (e.g. a fresh AQ_CONFIG_DIR, or a save made from another machine); the
// worst case of a stale miss is one redundant prompt, never data loss.
func namedLineagesPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "setup-lineages.json"), nil
}

// loadNamedLineages reads the local first-save cache, keyed by setup id
// (uuid). Any read/parse failure degrades to "nothing recorded yet" rather
// than erroring — it only ever gates a UX prompt, never the save itself.
func loadNamedLineages() map[string]bool {
	path, err := namedLineagesPath()
	if err != nil {
		return map[string]bool{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]bool{}
	}
	var m map[string]bool
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]bool{}
	}
	return m
}

// markLineageNamed records that setupID's save lineage has now been named, so
// later `aq save` runs stop prompting/defaulting a name for it. A failed
// write is silently ignored — at worst it re-prompts next time.
func markLineageNamed(setupID string) {
	path, err := namedLineagesPath()
	if err != nil {
		return
	}
	m := loadNamedLineages()
	m[setupID] = true
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}
