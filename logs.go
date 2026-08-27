package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Aquanodeio/aq/internal/config"
)

// logsOptions configures runLogs. logsCmd fills in the real environment.
type logsOptions struct {
	cred   *config.Credential
	target string
	run    string // "" → the most recent run
	dir    string // remote working directory the run was launched in
	lines  int
	follow bool
	list   bool
	print  bool
	out    io.Writer
	errOut io.Writer

	resolveAlias func(target string, errOut io.Writer) (string, error)
	handoff      func(args []string) error
}

// logsCmd parses `aq logs [name|id]`.
//
// The other half of `aq run --detach`: a detached run's output goes to a file
// on the box, and this is how you read it. Defaults to the most recent run,
// because "what is my training doing" is the question, not "what did run
// 20260826-141230 do".
func logsCmd(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	run := fs.String("run", "", "Read a specific run id (default: the most recent)")
	dir := fs.String("dir", "", "Working directory the run was launched in (default: "+defaultRemoteDir+")")
	lines := fs.Int("n", 200, "How many trailing lines to show")
	follow := fs.Bool("f", false, "Keep streaming as the run writes more")
	list := fs.Bool("list", false, "List this box's runs and their status instead of printing a log")
	printOnly := fs.Bool("print", false, "Print the ssh command that would run, and exit")

	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return fmt.Errorf("expected at most one deployment — got %s", strings.Join(positional, ", "))
	}
	var target string
	if len(positional) == 1 {
		target = positional[0]
	}
	if strings.HasPrefix(target, "-") {
		return fmt.Errorf("invalid deployment %q — it must not start with '-'", target)
	}
	if *lines < 0 {
		return fmt.Errorf("-n must not be negative, got %d", *lines)
	}
	if err := validateRunID(*run); err != nil {
		return err
	}

	var cred *config.Credential
	if !isHostTarget(target) {
		if cred, err = requireLogin(); err != nil {
			return err
		}
	}

	return runLogs(logsOptions{
		cred:   cred,
		target: target,
		run:    *run,
		dir:    *dir,
		lines:  *lines,
		follow: *follow,
		list:   *list,
		print:  *printOnly,
		out:    os.Stdout,
		errOut: os.Stderr,
	})
}

// validateRunID keeps a run id to the shape the box mints. The id is
// interpolated into a remote shell path, so anything outside this alphabet —
// a quote, a slash, a $ — is refused here rather than quoted and hoped for.
func validateRunID(id string) error {
	if id == "" {
		return nil
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
		default:
			return fmt.Errorf("invalid run id %q — run ids look like 20260826-141230", id)
		}
	}
	return nil
}

// runLogs resolves the box and streams the log.
func runLogs(opts logsOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.errOut == nil {
		opts.errOut = os.Stderr
	}
	if opts.resolveAlias == nil {
		cred := opts.cred
		opts.resolveAlias = func(target string, errOut io.Writer) (string, error) {
			return resolveSSHAlias(newControlClientOrNil(cred), target, "read logs from", errOut)
		}
	}
	if opts.handoff == nil {
		opts.handoff = execSSH
	}

	dir, err := validateRemoteDir(opts.dir)
	if err != nil {
		return err
	}

	alias, err := opts.resolveAlias(opts.target, opts.errOut)
	if err != nil {
		return err
	}

	var script string
	if opts.list {
		script = buildListRunsScript(dir)
	} else {
		script = buildTailScript(dir, opts.run, opts.lines, opts.follow)
	}
	args := buildSSHArgs(alias, "", nil, []string{script})

	if opts.print {
		fmt.Fprintln(opts.out, "ssh "+shellJoin(args))
		return nil
	}
	return opts.handoff(args)
}

// resolveRunDirSnippet is the shell that picks which run to read: the one named
// by --run, else the newest. Run ids are timestamps, so lexical sort is
// chronological.
func resolveRunDirSnippet(dir, run string) string {
	base := shellQuote(strings.TrimSuffix(dir, "/") + "/" + runsDirName)
	if run != "" {
		// validateRunID has already restricted this to [A-Za-z0-9_-].
		return "base=" + base + "\nd=\"$base/" + run + "\"\n" +
			"[ -d \"$d\" ] || { echo \"no run " + run + " on this box\" >&2; exit 3; }\n"
	}
	return "base=" + base + "\n" +
		"d=$(ls -1d \"$base\"/*/ 2>/dev/null | sort | tail -1)\n" +
		"[ -n \"$d\" ] || { echo 'no detached runs on this box — start one with `aq run --detach -- <cmd>`' >&2; exit 3; }\n"
}

// buildTailScript prints a run's log, prefixed by whether it is still running.
//
// The status line matters more than it looks: a log that stops moving is
// ambiguous on its own — finished, crashed, or just quiet — and that ambiguity
// is the whole reason a detached run needs a status file rather than only a
// log.
func buildTailScript(dir, run string, lines int, follow bool) string {
	s := resolveRunDirSnippet(dir, run)
	s += "if [ -f \"$d/status\" ]; then echo \"[$(basename \"$d\")] exited $(cat \"$d/status\")\" >&2; " +
		"elif [ -f \"$d/pid\" ] && kill -0 \"$(cat \"$d/pid\")\" 2>/dev/null; then echo \"[$(basename \"$d\")] running\" >&2; " +
		"else echo \"[$(basename \"$d\")] not running, no exit status recorded\" >&2; fi\n"
	s += "touch \"$d/log\" 2>/dev/null || true\n"
	tail := "tail -n " + strconv.Itoa(lines)
	if follow {
		tail += " -f"
	}
	return s + tail + " \"$d/log\"\n"
}

// buildListRunsScript renders one line per run: id, state, and the command.
func buildListRunsScript(dir string) string {
	base := shellQuote(strings.TrimSuffix(dir, "/") + "/" + runsDirName)
	return "base=" + base + "\n" +
		"[ -d \"$base\" ] || { echo 'no detached runs on this box' >&2; exit 3; }\n" +
		"for d in \"$base\"/*/; do\n" +
		"  [ -d \"$d\" ] || continue\n" +
		"  if [ -f \"$d/status\" ]; then st=\"exited $(cat \"$d/status\")\";\n" +
		"  elif [ -f \"$d/pid\" ] && kill -0 \"$(cat \"$d/pid\")\" 2>/dev/null; then st=running;\n" +
		"  else st=unknown; fi\n" +
		"  printf '%-18s  %-12s  %s\\n' \"$(basename \"$d\")\" \"$st\" \"$(head -1 \"$d/cmd\" 2>/dev/null)\"\n" +
		"done\n"
}
