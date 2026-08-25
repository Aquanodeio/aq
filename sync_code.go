package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// defaultRemoteDir is where a pushed working directory lands. /workspace is the
// path every provider image mounts persistent storage at, and the one the
// README already tells people to scp into — so a pushed tree survives the same
// save/restore cycle as anything else on the box.
const defaultRemoteDir = "/workspace"

// ignoreFileName is an optional per-project exclude list, one rsync-style
// pattern per line, `#` for comments.
const ignoreFileName = ".aqignore"

// defaultExcludes are never worth pushing to a GPU box: version-control
// metadata, virtualenvs and module trees that are platform-specific anyway, and
// editor/interpreter scratch. Dropping them is what keeps a first push seconds
// rather than minutes on a typical repo.
func defaultExcludes() []string {
	return []string{
		".git",
		".hg",
		".svn",
		".venv",
		"venv",
		"node_modules",
		"__pycache__",
		"*.pyc",
		".mypy_cache",
		".pytest_cache",
		".ruff_cache",
		".DS_Store",
		ignoreFileName,
	}
}

// transferPlan is the fully-resolved description of one push: which local
// directory goes to which remote directory over which transport, with argv
// already built. Producing it is pure, so the interesting decisions are
// testable without a box.
type transferPlan struct {
	mode      string // "rsync" or "tar"
	alias     string
	from      string // absolute local directory
	to        string // absolute remote directory
	del       bool
	argv      []string // local command to run (rsync, or the local half of tar)
	remoteCmd []string // ssh argv for the receiving end; empty in rsync mode
}

// describe renders the plan as the shell the user could have typed. `--print`
// prints this, and it is what the tests assert against.
func (p transferPlan) describe() string {
	local := shellJoin(p.argv)
	if p.mode == "rsync" {
		return local
	}
	return local + " | ssh " + shellJoin(p.remoteCmd)
}

// resolveExcludes assembles the exclude list: defaults (unless waived), then
// the project's .aqignore, then whatever --exclude flags were passed. Later
// entries are additive — there is deliberately no un-exclude, because rsync and
// tar disagree about negation and a pattern language that behaves differently
// per transport would be worse than none.
func resolveExcludes(from string, extra []string, useDefaults bool) ([]string, error) {
	var out []string
	if useDefaults {
		out = append(out, defaultExcludes()...)
	}
	fromFile, err := readIgnoreFile(filepath.Join(from, ignoreFileName))
	if err != nil {
		return nil, err
	}
	out = append(out, fromFile...)
	out = append(out, extra...)
	return out, nil
}

// readIgnoreFile parses .aqignore. A missing file is not an error; an
// unreadable one is, because silently pushing something the user asked to keep
// local is the failure that matters here.
func readIgnoreFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("could not read %s: %w", path, err)
	}
	return out, nil
}

// buildPlan picks a transport and builds its argv.
//
// rsync is preferred because the push loop is re-run constantly — edit, push,
// run — and only rsync sends deltas. It needs the binary on BOTH ends, and the
// Docker-pool base image is not guaranteed to carry it, so tar-over-ssh is the
// fallback: it needs only tar, which every image has. The cost is that tar
// re-sends the whole tree every time and cannot express --delete.
func buildPlan(alias, from, to string, excludes []string, del, remoteHasRsync bool) (transferPlan, error) {
	p := transferPlan{alias: alias, from: from, to: to, del: del}

	if _, err := exec.LookPath("rsync"); err == nil && remoteHasRsync {
		p.mode = "rsync"
		p.argv = buildRsyncArgs(alias, from, to, excludes, del)
		return p, nil
	}

	if del {
		// Better to refuse than to quietly leave deleted files on the box: the
		// user asked for the remote tree to mirror the local one, and tar cannot
		// do that.
		return transferPlan{}, fmt.Errorf("--delete needs rsync on both ends, and it is missing — install rsync on the box (or drop --delete)")
	}
	p.mode = "tar"
	p.argv = buildTarArgs(from, excludes, tarSupportsNoXattrs())
	p.remoteCmd = buildSSHArgs(alias, "", nil, []string{remoteExtractCmd(to)})
	return p, nil
}

// buildRsyncArgs assembles rsync's argv (including argv[0]).
//
// The trailing slash on the source is load-bearing: "dir/" means *the contents
// of* dir, so ./src pushed to /workspace lands as /workspace/main.py, not
// /workspace/src/main.py. --rsync-path creates the destination first, since
// rsync itself will not mkdir a missing parent and a fresh box has no
// /workspace/<project>.
func buildRsyncArgs(alias, from, to string, excludes []string, del bool) []string {
	// Every flag here must exist in BOTH GNU rsync and openrsync: recent macOS
	// ships openrsync in rsync's place, and GNU-only niceties (--info=stats1,
	// --info=progress2) make it exit with a usage dump. --stats is in both.
	args := []string{"rsync", "-az", "--stats", "--rsync-path", "mkdir -p " + shellQuote(to) + " && rsync"}
	if del {
		args = append(args, "--delete")
	}
	for _, e := range excludes {
		args = append(args, "--exclude", e)
	}
	return append(args, withTrailingSlash(from), alias+":"+withTrailingSlash(to))
}

// buildTarArgs assembles the local half of the tar fallback: stream the tree to
// stdout. -C plus "." rather than naming the directory keeps the archive's
// paths relative, so it extracts *into* the destination the same way the rsync
// trailing slash does.
func buildTarArgs(from string, excludes []string, noXattrs bool) []string {
	args := []string{"tar", "czf", "-"}
	if noXattrs {
		// macOS tags files it downloaded with a com.apple.provenance xattr, which
		// bsdtar faithfully archives as a LIBARCHIVE.xattr.* header — and GNU tar
		// on the box then prints "Ignoring unknown extended header keyword" once
		// per file. The transfer succeeds either way, but a wall of tar warnings
		// is indistinguishable from a broken push.
		args = append(args, "--no-xattrs")
	}
	args = append(args, "-C", from)
	for _, e := range excludes {
		args = append(args, "--exclude", e)
	}
	return append(args, ".")
}

// remoteExtractCmd is the receiving half of the tar fallback. It is passed to
// ssh as a single argument so the remote login shell parses it as one command
// line, which is also why both paths are shell-quoted here.
func remoteExtractCmd(to string) string {
	q := shellQuote(to)
	return "mkdir -p " + q + " && tar xzf - -C " + q
}

// tarSupportsNoXattrs reports whether the local tar accepts --no-xattrs.
// bsdtar and GNU tar 1.27+ both do, but the flag is not universal, and an
// unrecognized one would fail the push outright — a worse outcome than the
// cosmetic warnings it suppresses.
func tarSupportsNoXattrs() bool {
	return exec.Command("tar", "--no-xattrs", "-cf", os.DevNull, "-T", os.DevNull).Run() == nil
}

// probeRemoteRsync asks the box whether it has rsync. One extra round trip is
// cheap next to a file transfer, and it makes the transport choice deterministic
// instead of inferring it from a failed rsync's exit code — which is ambiguous,
// since rsync exits non-zero for network trouble too.
func probeRemoteRsync(alias string) bool {
	cmd := exec.Command("ssh", buildSSHArgs(alias, "", nil, []string{"command -v rsync >/dev/null 2>&1"})...)
	return cmd.Run() == nil
}

// runTransfer executes a plan, streaming progress to the user's terminal.
func runTransfer(p transferPlan, errOut io.Writer) error {
	if p.mode == "rsync" {
		cmd := exec.Command(p.argv[0], p.argv[1:]...)
		cmd.Stdout = errOut
		cmd.Stderr = errOut
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("rsync failed: %w", err)
		}
		return nil
	}

	// tar | ssh, wired in-process rather than through `sh -c` so no local path
	// ever goes through a shell parser.
	local := exec.Command(p.argv[0], p.argv[1:]...)
	// COPYFILE_DISABLE stops macOS bsdtar from archiving resource forks and
	// xattrs. Without it every pushed file arrives with an AppleDouble "._name"
	// sibling next to it and GNU tar on the box prints an "unknown extended
	// header keyword" warning per file — junk in the user's /workspace, and
	// noise that reads like the push failed. Harmless on Linux, which has no
	// such thing to disable.
	local.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	remote := exec.Command("ssh", p.remoteCmd...)

	pipe, err := local.StdoutPipe()
	if err != nil {
		return fmt.Errorf("could not build the transfer pipe: %w", err)
	}
	remote.Stdin = pipe
	local.Stderr = errOut
	remote.Stdout = errOut
	remote.Stderr = errOut

	if err := remote.Start(); err != nil {
		return fmt.Errorf("could not start ssh: %w", err)
	}
	if err := local.Start(); err != nil {
		return fmt.Errorf("could not start tar: %w", err)
	}
	tarErr := local.Wait()
	// Close the write end so the remote tar sees EOF and exits, whether or not
	// the local tar succeeded — otherwise a failed tar leaves ssh blocked on a
	// pipe that will never close.
	pipe.Close()
	sshErr := remote.Wait()

	if tarErr != nil {
		return fmt.Errorf("tar failed: %w", tarErr)
	}
	if sshErr != nil {
		return fmt.Errorf("the box could not unpack the upload: %w", sshErr)
	}
	return nil
}

// withTrailingSlash normalizes a directory for rsync/scp "contents of"
// semantics.
func withTrailingSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

// shellQuote wraps a string for safe interpretation by a POSIX shell.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin renders an argv the way a user would have to type it.
func shellJoin(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$&|;<>()*?[]{}#~") {
			out = append(out, shellQuote(a))
			continue
		}
		out = append(out, a)
	}
	return strings.Join(out, " ")
}

// resolveLocalDir turns --from into an absolute path, failing early if it is
// not a directory the user can read.
func resolveLocalDir(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("could not resolve %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("could not read %s: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory — aq push sends a directory tree", abs)
	}
	return abs, nil
}

// validateRemoteDir rejects a destination that is not absolute. A relative one
// would resolve against the ssh login's home, which differs per provider image
// — so the same command would put files in different places on different boxes.
func validateRemoteDir(dir string) (string, error) {
	if dir == "" {
		return defaultRemoteDir, nil
	}
	if !strings.HasPrefix(dir, "/") {
		return "", fmt.Errorf("--to must be an absolute path on the box, got %q", dir)
	}
	return strings.TrimSuffix(dir, "/"), nil
}
