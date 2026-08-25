package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestValidateRunIDRefusesShellMetacharacters: the run id is interpolated
// straight into a remote shell path, so anything outside the minted alphabet
// is refused here rather than quoted and hoped for.
func TestValidateRunIDRefusesShellMetacharacters(t *testing.T) {
	if err := validateRunID(""); err != nil {
		t.Fatalf("empty means 'the latest run': %v", err)
	}
	if err := validateRunID("20260826-141230"); err != nil {
		t.Fatalf("a real id must pass: %v", err)
	}
	for _, bad := range []string{"../../etc", "a b", "x;rm -rf /", "$(whoami)", "`id`", "a'b", "a/b"} {
		if err := validateRunID(bad); err == nil {
			t.Fatalf("want %q refused", bad)
		}
	}
}

func TestBuildTailScriptDefaultsToTheNewestRun(t *testing.T) {
	s := buildTailScript("/workspace", "", 200, false)
	if !strings.Contains(s, "sort | tail -1") {
		t.Fatal("with no --run the newest run must be picked; ids are timestamps so lexical sort is chronological")
	}
	if !strings.Contains(s, "tail -n 200") {
		t.Fatalf("got %s", s)
	}
	if strings.Contains(s, "tail -n 200 -f") {
		t.Fatal("-f must not be set unless asked for")
	}
	if !strings.Contains(s, "/workspace/"+runsDirName) {
		t.Fatalf("runs live under the working directory, got %s", s)
	}
}

func TestBuildTailScriptFollowAndSpecificRun(t *testing.T) {
	s := buildTailScript("/data", "20260826-141230", 50, true)
	if !strings.Contains(s, "tail -n 50 -f") {
		t.Fatalf("got %s", s)
	}
	if !strings.Contains(s, `d="$base/20260826-141230"`) {
		t.Fatalf("want the named run selected, got %s", s)
	}
	if !strings.Contains(s, "/data/"+runsDirName) {
		t.Fatalf("--dir must move the runs directory, got %s", s)
	}
}

// TestBuildTailScriptReportsRunState: a log that stops moving is ambiguous —
// finished, crashed, or just quiet. The status line is what disambiguates it,
// and it goes to stderr so piping the log stays clean.
func TestBuildTailScriptReportsRunState(t *testing.T) {
	s := buildTailScript("/workspace", "", 10, false)
	for _, want := range []string{"exited", "running", "not running"} {
		if !strings.Contains(s, want) {
			t.Fatalf("want a %q state reported, got %s", want, s)
		}
	}
	if !strings.Contains(s, `kill -0`) {
		t.Fatal("liveness must be probed, not assumed from the log")
	}
}

func TestBuildListRunsScript(t *testing.T) {
	s := buildListRunsScript("/workspace")
	if !strings.Contains(s, "/workspace/"+runsDirName) {
		t.Fatalf("got %s", s)
	}
	if !strings.Contains(s, "no detached runs on this box") {
		t.Fatal("an empty runs directory needs a real message, not empty output")
	}
}

// TestRunLogsPrintShowsTheCommand keeps --print honest and proves the target
// resolution runs before any shell is built.
func TestRunLogsPrintShowsTheCommand(t *testing.T) {
	var out bytes.Buffer
	err := runLogs(logsOptions{
		target:       "box",
		lines:        20,
		print:        true,
		out:          &out,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		handoff: func([]string) error {
			t.Fatal("--print must not connect")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "ssh aq-box") || !strings.Contains(out.String(), "tail -n 20") {
		t.Fatalf("got %s", out.String())
	}
}

func TestRunLogsRejectsRelativeDir(t *testing.T) {
	err := runLogs(logsOptions{
		dir:          "work",
		out:          io.Discard,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		handoff:      func([]string) error { return nil },
	})
	if err == nil {
		t.Fatal("want an error for a relative --dir")
	}
}
