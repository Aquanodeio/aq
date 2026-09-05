package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func hasLocalRsync(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("no local rsync — transport choice is untestable here")
	}
}

// TestBuildRsyncArgsSendsContentsNotTheDirectory pins the trailing slash. Drop
// it and `aq push --from ./src --to /workspace` lands the tree at
// /workspace/src instead of /workspace, so every path the user's code refers to
// gains a level and nothing they run finds its own files.
func TestBuildRsyncArgsSendsContentsNotTheDirectory(t *testing.T) {
	args := buildRsyncArgs("aq-box", "/home/me/proj", "/workspace", nil, false)
	got := strings.Join(args, " ")

	if !strings.Contains(got, "/home/me/proj/ aq-box:/workspace/") {
		t.Fatalf("source and destination must both end in a slash, got: %s", got)
	}
	// A fresh box has no /workspace/<project>; rsync will not create a missing
	// parent on its own, so the mkdir must ride along.
	if !strings.Contains(got, "--rsync-path mkdir -p '/workspace' && rsync") {
		t.Fatalf("expected --rsync-path to mkdir the destination, got: %s", got)
	}
}

// TestBuildPlanFallsBackToTarWithoutRemoteRsync is the load-bearing provider
// case: the Docker-pool base image is not guaranteed to carry rsync, and an aq
// that assumed it would fail every push on those pools.
func TestBuildPlanFallsBackToTarWithoutRemoteRsync(t *testing.T) {
	p, err := buildPlan("aq-box", "/home/me/proj", "/workspace", []string{".git"}, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.mode != "tar" {
		t.Fatalf("want tar mode when the box has no rsync, got %q", p.mode)
	}
	local := strings.Join(p.argv, " ")
	if !strings.Contains(local, "-C /home/me/proj") || !strings.HasSuffix(local, " .") {
		t.Fatalf("tar must archive relative paths so it extracts into the destination, got: %s", local)
	}
	if !strings.Contains(local, "--exclude .git") {
		t.Fatalf("excludes must reach tar, got: %s", local)
	}
	remote := strings.Join(p.remoteCmd, " ")
	if !strings.Contains(remote, "mkdir -p '/workspace' && tar xzf - -C '/workspace'") {
		t.Fatalf("remote half must mkdir then extract, got: %s", remote)
	}
}

func TestBuildPlanPrefersRsyncWhenBothEndsHaveIt(t *testing.T) {
	hasLocalRsync(t)
	p, err := buildPlan("aq-box", "/home/me/proj", "/workspace", nil, false, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.mode != "rsync" {
		t.Fatalf("want rsync mode, got %q", p.mode)
	}
	if len(p.remoteCmd) != 0 {
		t.Fatalf("rsync mode drives its own transport, want no remote command, got %v", p.remoteCmd)
	}
}

// TestBuildPlanRefusesDeleteWithoutRsync: silently downgrading to tar would
// leave files the user deleted locally still sitting on the box, and a stale
// module the next run happily imports is worse than a refusal.
func TestBuildPlanRefusesDeleteWithoutRsync(t *testing.T) {
	_, err := buildPlan("aq-box", "/home/me/proj", "/workspace", nil, true, false)
	if err == nil {
		t.Fatal("want an error when --delete cannot be honoured")
	}
	if !strings.Contains(err.Error(), "--delete") {
		t.Fatalf("the error must name the flag that cannot be honoured, got: %v", err)
	}
}

func TestResolveExcludes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ignoreFileName),
		[]byte("# checkpoints are huge\ncheckpoints/\n\n*.safetensors\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveExcludes(dir, []string{"scratch"}, true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{".git", "node_modules", "checkpoints/", "*.safetensors", "scratch"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("want %q in the exclude list, got: %s", want, joined)
		}
	}
	if strings.Contains(joined, "# checkpoints") {
		t.Fatalf("comments must not become patterns, got: %s", joined)
	}

	bare, err := resolveExcludes(dir, nil, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(strings.Join(bare, " "), ".git") {
		t.Fatalf("--no-default-excludes must drop the defaults, got: %v", bare)
	}
	// .aqignore is the user's own list, so it survives --no-default-excludes.
	if !strings.Contains(strings.Join(bare, " "), "checkpoints/") {
		t.Fatalf(".aqignore must still apply, got: %v", bare)
	}
}

// TestResolveExcludesSkipsCredentialsByDefault is #779: a push from a directory
// holding a real .env and an SSH private key must not need a hand-maintained
// .aqignore to keep them off the wire.
func TestResolveExcludesSkipsCredentialsByDefault(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveExcludes(dir, nil, true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{".env*", "*.pem", "*.key", "id_rsa*", "id_ecdsa*", "id_ed25519*", "*.p12", "*.pfx", ".aws", ".ssh", ".netrc", "credentials.json", ".npmrc", ".pypirc"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("want credential pattern %q in the exclude list, got: %s", want, joined)
		}
	}
}

// TestResolveExcludesIncludeSecretsIsTheOnlyWaiver: --include-secrets and
// --no-default-excludes must be independent. Turning off build-noise
// filtering must not silently also start sending .env, and vice versa.
func TestResolveExcludesIncludeSecretsIsTheOnlyWaiver(t *testing.T) {
	dir := t.TempDir()

	waived, err := resolveExcludes(dir, nil, true, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(waived, " ")
	if strings.Contains(joined, ".env*") || strings.Contains(joined, "id_rsa*") {
		t.Fatalf("--include-secrets must drop the credential patterns, got: %s", joined)
	}
	if !strings.Contains(joined, ".git") {
		t.Fatalf("--include-secrets must not also waive the build-noise defaults, got: %s", joined)
	}

	noDefaults, err := resolveExcludes(dir, nil, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.Join(noDefaults, " "), ".env*") {
		t.Fatalf("--no-default-excludes must not also send credentials, got: %v", noDefaults)
	}
}

// TestCredentialExcludesReachBothTransports: buildRsyncArgs and buildTarArgs
// are built separately from the same resolved exclude list, so a pattern
// missing from one but not the other would ship secrets on whichever
// transport a box happens to pick.
func TestCredentialExcludesReachBothTransports(t *testing.T) {
	dir := t.TempDir()
	excludes, err := resolveExcludes(dir, nil, true, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tarArgs := strings.Join(buildTarArgs(dir, excludes, false), " ")
	rsyncArgs := strings.Join(buildRsyncArgs("aq-box", dir, "/workspace", excludes, false), " ")
	for _, want := range []string{"--exclude .env*", "--exclude id_ed25519*", "--exclude credentials.json"} {
		if !strings.Contains(tarArgs, want) {
			t.Fatalf("tar argv missing %q: %s", want, tarArgs)
		}
		if !strings.Contains(rsyncArgs, want) {
			t.Fatalf("rsync argv missing %q: %s", want, rsyncArgs)
		}
	}
}

// TestScanCredentialMatchesFindsSecretsAndPrunesBuildNoise: the scan drives the
// user-facing skip summary, so it has to find what resolveExcludes actually
// hides, and it must not descend into node_modules doing it.
func TestScanCredentialMatchesFindsSecretsAndPrunesBuildNoise(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		".env":                   "SECRET=1\n",
		"id_ed25519":             "-----BEGIN OPENSSH PRIVATE KEY-----\n",
		"aqcfg/credentials.json": `{"apiKey":"x"}`,
		"train.py":               "print(1)\n",
		"node_modules/x/.env":    "SECRET=2\n",
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	matches, err := scanCredentialMatches(dir, credentialExcludes(), defaultExcludes())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.Join(matches, " ")
	for _, want := range []string{".env", "id_ed25519", filepath.Join("aqcfg", "credentials.json")} {
		if !strings.Contains(got, want) {
			t.Fatalf("want %q reported, got: %v", want, matches)
		}
	}
	if strings.Contains(got, "node_modules") {
		t.Fatalf("must not descend into a pruned build-noise directory, got: %v", matches)
	}
}

// TestValidateRemoteDirRejectsRelative: a relative destination resolves against
// the ssh login's home, which differs per provider image — so the same command
// would put files somewhere different on different boxes.
func TestValidateRemoteDirRejectsRelative(t *testing.T) {
	if _, err := validateRemoteDir("work"); err == nil {
		t.Fatal("want an error for a relative --to")
	}
	got, err := validateRemoteDir("")
	if err != nil || got != defaultRemoteDir {
		t.Fatalf("empty --to must default to %s, got %q (%v)", defaultRemoteDir, got, err)
	}
	if got, _ := validateRemoteDir("/workspace/"); got != "/workspace" {
		t.Fatalf("a trailing slash must be normalized away, got %q", got)
	}
}

func TestResolveLocalDirRejectsAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "train.py")
	if err := os.WriteFile(file, []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLocalDir(file); err == nil {
		t.Fatal("want an error when --from is a file")
	}
	got, err := resolveLocalDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("want an absolute path, got %q", got)
	}
}

func TestShellQuoteHandlesQuotes(t *testing.T) {
	if got := shellQuote("/it's/here"); got != `'/it'\''s/here'` {
		t.Fatalf("got %s", got)
	}
	if got := shellQuote(""); got != "''" {
		t.Fatalf("got %s", got)
	}
}
