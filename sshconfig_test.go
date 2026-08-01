package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// The user's own ssh_config is the highest-consequence file aq touches — a
// mangled one locks them out of every host they use. These cover the three
// properties that keep it safe: everything outside the markers survives byte for
// byte, the region lands at the TOP (first-match-wins), and a config with
// damaged markers is refused rather than guessed at.

func TestApplyIncludeRegionPrependsAndPreservesExistingBytes(t *testing.T) {
	existing := "Host bastion\n\tUser ops\n\tProxyJump none\n\n# trailing comment, no newline"

	got, err := applyIncludeRegion(existing, "/home/u/.ssh/aquanode.config")
	if err != nil {
		t.Fatalf("applyIncludeRegion: %v", err)
	}

	if !strings.HasPrefix(got, beginMarker) {
		t.Errorf("region must be at the top (ssh_config is first-match-wins); got:\n%s", got)
	}
	if !strings.HasSuffix(got, existing) {
		t.Errorf("existing content must survive byte for byte; got:\n%q", got)
	}
	if !strings.Contains(got, "Include /home/u/.ssh/aquanode.config") {
		t.Errorf("missing Include line; got:\n%s", got)
	}
}

func TestApplyIncludeRegionIsIdempotent(t *testing.T) {
	const path = "/home/u/.ssh/aquanode.config"
	once, err := applyIncludeRegion("Host a\n\tUser u\n", path)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	twice, err := applyIncludeRegion(once, path)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if once != twice {
		t.Errorf("second pass changed the file:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

func TestApplyIncludeRegionRewritesStaleRegionInPlace(t *testing.T) {
	stale := beginMarker + "\nInclude /old/path\n" + endMarker + "\n\nHost keep\n\tUser me\n"

	got, err := applyIncludeRegion(stale, "/new/path")
	if err != nil {
		t.Fatalf("applyIncludeRegion: %v", err)
	}
	if strings.Contains(got, "/old/path") {
		t.Errorf("stale Include should have been replaced; got:\n%s", got)
	}
	if !strings.Contains(got, "Include /new/path") {
		t.Errorf("new Include missing; got:\n%s", got)
	}
	if !strings.Contains(got, "Host keep\n\tUser me\n") {
		t.Errorf("content after the region must be preserved; got:\n%s", got)
	}
	if n := strings.Count(got, beginMarker); n != 1 {
		t.Errorf("expected exactly one BEGIN marker, got %d:\n%s", n, got)
	}
}

func TestApplyIncludeRegionRefusesMalformedMarkers(t *testing.T) {
	cases := map[string]string{
		"begin without end": beginMarker + "\nInclude /x\n\nHost a\n",
		"end without begin": "Host a\n" + endMarker + "\n",
		"end before begin":  endMarker + "\nInclude /x\n" + beginMarker + "\n",
		"duplicate begin":   beginMarker + "\n" + beginMarker + "\nInclude /x\n" + endMarker + "\n",
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := applyIncludeRegion(existing, "/x"); err == nil {
				t.Error("expected a refusal — aq must never guess at repairing a user's ssh config")
			}
		})
	}
}

func TestEnsureIncludeRegionCreatesConfigWhenAbsent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := ensureIncludeRegion(); err != nil {
		t.Fatalf("ensureIncludeRegion: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "Include "+filepath.Join(home, ".ssh", managedConfigName)) {
		t.Errorf("Include line missing; got:\n%s", data)
	}
	info, err := os.Stat(filepath.Join(home, ".ssh"))
	if err != nil {
		t.Fatalf("stat .ssh: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("~/.ssh must be 0700 or ssh refuses it, got %#o", perm)
	}
}

func TestAliasFor(t *testing.T) {
	cases := []struct {
		name string
		id   int
		want string
	}{
		{"inference-box", 7, "aq-inference-box"},
		{"Inference Box", 7, "aq-inference-box"},
		{"ticket-422-423-sshtest", 12, "aq-ticket-422-423-sshtest"},
		{"", 42, "aq-42"},
		{"!!!", 42, "aq-42"},
		{"   ", 42, "aq-42"},
		{"日本語", 42, "aq-42"},
		{"a__b--c", 9, "aq-a-b-c"},
		{"Fast RTX 4090 from Iceland", 3, "aq-fast-rtx-4090-from-iceland"},
		{strings.Repeat("x", 60), 5, "aq-" + strings.Repeat("x", 40)},
	}
	for _, c := range cases {
		if got := aliasFor(c.name, c.id); got != c.want {
			t.Errorf("aliasFor(%q, %d) = %q, want %q", c.name, c.id, got, c.want)
		}
	}
}

func TestEntriesForEmitsBothAliasesAndDedupes(t *testing.T) {
	named := api.Deployment{ID: 7, Name: "box", AppURL: "http://203.0.113.9:22"}
	entries := entriesFor(named, "/k", "/kh")
	if len(entries) != 2 || entries[0].Alias != "aq-box" || entries[1].Alias != "aq-7" {
		t.Fatalf("named box should get the name alias and the stable id alias, got %+v", entries)
	}

	unnamed := api.Deployment{ID: 7, AppURL: "http://203.0.113.9:22"}
	if entries := entriesFor(unnamed, "/k", "/kh"); len(entries) != 1 || entries[0].Alias != "aq-7" {
		t.Errorf("unnamed box should get exactly one alias, got %+v", entries)
	}

	if entries := entriesFor(api.Deployment{ID: 7}, "/k", "/kh"); entries != nil {
		t.Errorf("a box with no address should yield no stanza, got %+v", entries)
	}
}

func TestRenderManagedConfigUsesAbsolutePathsAndDedicatedKnownHosts(t *testing.T) {
	got := renderManagedConfig([]sshEntry{{
		Alias: "aq-box", HostName: "203.0.113.9", Port: "2222",
		IdentityFile: "/home/u/.ssh/aquanode_ed25519", KnownHosts: "/home/u/.ssh/aquanode_known_hosts",
	}})

	for _, want := range []string{
		"Host aq-box",
		"Port 2222",
		"User root",
		"IdentityFile /home/u/.ssh/aquanode_ed25519",
		"IdentitiesOnly yes",
		"UserKnownHostsFile /home/u/.ssh/aquanode_known_hosts",
		"StrictHostKeyChecking accept-new",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config missing %q; got:\n%s", want, got)
		}
	}
	// A tilde would be expanded by ssh from the passwd entry, not from $HOME —
	// the two disagree inside a container or CI job, leaving IdentityFile
	// pointing at a key that isn't there.
	if strings.Contains(got, "~/") {
		t.Errorf("paths must be absolute, not tilde'd; got:\n%s", got)
	}
	// Never the industry-standard bad habit: that suppresses the prompt by
	// permanently disabling MITM detection.
	if strings.Contains(got, "StrictHostKeyChecking no") || strings.Contains(got, "/dev/null") {
		t.Errorf("must not disable host key checking; got:\n%s", got)
	}
}
