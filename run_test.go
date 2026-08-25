package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// stubResolve returns a fixed alias without touching the network.
func stubResolve(alias string) func(string, io.Writer) (string, error) {
	return func(string, io.Writer) (string, error) { return alias, nil }
}

// TestRunPushesThenRunsInTheDestination is the whole point of `aq run`: the
// command must execute where the files just landed. Defaulting --dir to the
// login home instead would make `aq run -- python train.py` fail with "no such
// file" on a box that demonstrably has train.py.
func TestRunPushesThenRunsInTheDestination(t *testing.T) {
	var pushed string
	var sshArgs []string

	err := runRun(runOptions{
		target:       "box",
		command:      []string{"python", "train.py"},
		push:         pushOptions{to: "/workspace/proj"},
		out:          io.Discard,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		doPush: func(alias string, opts pushOptions) error {
			pushed = alias + ":" + opts.to
			return nil
		},
		handoff: func(args []string) error { sshArgs = args; return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushed != "aq-box:/workspace/proj" {
		t.Fatalf("want a push to the box first, got %q", pushed)
	}
	got := strings.Join(sshArgs, " ")
	if got != "aq-box cd '/workspace/proj' && python train.py" {
		t.Fatalf("want the command run in the push destination, got: %s", got)
	}
}

// TestRunNoPushSkipsTheTransfer covers the re-run case — the code is already
// there and the user just wants another execution.
func TestRunNoPushSkipsTheTransfer(t *testing.T) {
	pushCalls := 0
	var sshArgs []string

	err := runRun(runOptions{
		command:      []string{"nvidia-smi"},
		noPush:       true,
		out:          io.Discard,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		doPush:       func(string, pushOptions) error { pushCalls++; return nil },
		handoff:      func(args []string) error { sshArgs = args; return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushCalls != 0 {
		t.Fatalf("--no-push must not transfer, got %d calls", pushCalls)
	}
	if !strings.Contains(strings.Join(sshArgs, " "), "nvidia-smi") {
		t.Fatalf("want the command to still run, got: %v", sshArgs)
	}
}

// TestRunDirOverridesPushDestination: --dir lets you push a repo root but run
// inside a subdirectory of it.
func TestRunDirOverridesPushDestination(t *testing.T) {
	var sshArgs []string
	err := runRun(runOptions{
		command:      []string{"pytest"},
		dir:          "/workspace/tests",
		push:         pushOptions{to: "/workspace"},
		out:          io.Discard,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		doPush:       func(string, pushOptions) error { return nil },
		handoff:      func(args []string) error { sshArgs = args; return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(strings.Join(sshArgs, " "), "cd '/workspace/tests'") {
		t.Fatalf("want --dir to win, got: %v", sshArgs)
	}
}

// TestRunDoesNotExecWhenThePushFails: running the old code after a failed
// upload is how you debug a bug you already fixed.
func TestRunDoesNotExecWhenThePushFails(t *testing.T) {
	handoffCalls := 0
	err := runRun(runOptions{
		command:      []string{"python", "train.py"},
		out:          io.Discard,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		doPush:       func(string, pushOptions) error { return io.ErrUnexpectedEOF },
		handoff:      func([]string) error { handoffCalls++; return nil },
	})
	if err == nil {
		t.Fatal("want the push failure to surface")
	}
	if handoffCalls != 0 {
		t.Fatalf("must not run the command after a failed push, got %d calls", handoffCalls)
	}
}

// TestRunPreservesShellSyntax: the command is joined into one string for the
// remote login shell, so a pipe or a redirect the user typed still means what
// they meant.
func TestRunPreservesShellSyntax(t *testing.T) {
	got := remoteCommand("/workspace", []string{"python", "train.py", "|", "tee", "log.txt"})
	if got != "cd '/workspace' && python train.py | tee log.txt" {
		t.Fatalf("got %q", got)
	}
}

// TestPushPrintShowsTheRealCommand keeps --print honest: it must render the
// transport aq actually chose, not a generic template.
func TestPushPrintShowsTheRealCommand(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()

	err := pushToAlias("aq-box", pushOptions{
		from:       dir,
		to:         "/workspace",
		printOnly:  true,
		out:        &out,
		errOut:     io.Discard,
		probeRsync: func(string) bool { return false },
		transfer: func(transferPlan, io.Writer) error {
			t.Fatal("--print must not transfer anything")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "tar czf -") || !strings.Contains(got, "| ssh aq-box") {
		t.Fatalf("want the tar pipeline rendered, got: %s", got)
	}
}

// TestPushRunsTheResolvedPlan checks the ordinary path end to end, minus the
// process execution.
func TestPushRunsTheResolvedPlan(t *testing.T) {
	dir := t.TempDir()
	var got transferPlan

	err := pushToAlias("aq-box", pushOptions{
		from:       dir,
		errOut:     io.Discard,
		out:        io.Discard,
		probeRsync: func(string) bool { return false },
		transfer:   func(p transferPlan, _ io.Writer) error { got = p; return nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.mode != "tar" || got.to != defaultRemoteDir {
		t.Fatalf("want a tar push to %s, got %+v", defaultRemoteDir, got)
	}
	if !strings.Contains(strings.Join(got.argv, " "), "--exclude .git") {
		t.Fatalf("default excludes must apply, got: %v", got.argv)
	}
}
