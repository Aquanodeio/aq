package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// seedTree builds a directory that exercises the excludes: real source, a
// nested package, and the junk a push must leave behind.
func seedTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"train.py":                  "print('hi')\n",
		"pkg/model.py":              "W = 1\n",
		".git/HEAD":                 "ref: refs/heads/main\n",
		"__pycache__/train.cpython": "junk\n",
		"node_modules/x/index.js":   "junk\n",
		"data/keep.txt":             "keep\n",
		".aqignore":                 "data\n",
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
	return dir
}

// treeOf lists the relative file paths under root, for comparing what actually
// arrived against what should have.
func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// wantTree is what both transports must deliver: source in, junk out, and the
// paths flattened into the destination rather than nested under the source
// directory's own name.
var wantTree = []string{"pkg/model.py", "train.py"}

// TestTarTransportDeliversTheRightTree runs the real local tar argv aq builds
// and extracts it the way the box would. This is the transport that runs on
// every Docker-pool provider, so "the argv looks right" is not enough — the
// bytes have to land in the right place with the right things missing.
func TestTarTransportDeliversTheRightTree(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("no tar")
	}
	from := seedTree(t)
	to := t.TempDir()

	excludes, err := resolveExcludes(from, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	argv := buildTarArgs(from, excludes, tarSupportsNoXattrs())

	archive := filepath.Join(t.TempDir(), "push.tgz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		f.Close()
		t.Fatalf("tar failed: %v", err)
	}
	f.Close()

	if out, err := exec.Command("tar", "xzf", archive, "-C", to).CombinedOutput(); err != nil {
		t.Fatalf("extract failed: %v — %s", err, out)
	}

	got := treeOf(t, to)
	if strings.Join(got, ",") != strings.Join(wantTree, ",") {
		t.Fatalf("tar delivered %v, want %v", got, wantTree)
	}
}

// TestRsyncTransportDeliversTheRightTree runs the real rsync argv against a
// local destination — same flags, same excludes, same trailing-slash semantics,
// only the alias: prefix dropped.
func TestRsyncTransportDeliversTheRightTree(t *testing.T) {
	hasLocalRsync(t)
	from := seedTree(t)
	to := t.TempDir()

	excludes, err := resolveExcludes(from, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	argv := buildRsyncArgs("aq-box", from, to, excludes, false)
	// Drop --rsync-path (a remote-only concern) and retarget the destination
	// locally; everything else is byte-identical to a real push.
	local := []string{}
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--rsync-path" {
			i++
			continue
		}
		local = append(local, argv[i])
	}
	local[len(local)-1] = withTrailingSlash(to)

	if out, err := exec.Command(local[0], local[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("rsync failed: %v — %s", err, out)
	}

	got := treeOf(t, to)
	if strings.Join(got, ",") != strings.Join(wantTree, ",") {
		t.Fatalf("rsync delivered %v, want %v", got, wantTree)
	}
}

// TestRsyncDeletePrunesTheRemoteTree proves --delete does what the flag claims
// — the reason buildPlan refuses to silently downgrade it to tar.
func TestRsyncDeletePrunesTheRemoteTree(t *testing.T) {
	hasLocalRsync(t)
	from := t.TempDir()
	to := t.TempDir()
	if err := os.WriteFile(filepath.Join(from, "a.py"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file that exists only on the "box" — a module the user deleted locally.
	if err := os.WriteFile(filepath.Join(to, "stale.py"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	argv := buildRsyncArgs("aq-box", from, to, nil, true)
	local := []string{}
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--rsync-path" {
			i++
			continue
		}
		local = append(local, argv[i])
	}
	local[len(local)-1] = withTrailingSlash(to)

	if out, err := exec.Command(local[0], local[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("rsync failed: %v — %s", err, out)
	}
	if got := treeOf(t, to); strings.Join(got, ",") != "a.py" {
		t.Fatalf("--delete must prune stale.py, got %v", got)
	}
}
