package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestValidateGPUCount(t *testing.T) {
	if err := validateGPUCount(0); err != nil {
		t.Fatalf("0 is the unset sentinel, not an error: %v", err)
	}
	if err := validateGPUCount(8); err != nil {
		t.Fatalf("the cap itself must be allowed: %v", err)
	}
	for _, n := range []int{-1, 9, 1000} {
		err := validateGPUCount(n)
		if err == nil {
			t.Fatalf("want an error for --gpus %d", n)
		}
		if !strings.Contains(err.Error(), "--gpus") {
			t.Fatalf("the error must name the flag, got: %v", err)
		}
	}
}

// TestDetachedScriptSurvivesDisconnect pins the three things that make a
// detached run actually detached. Drop any one and the run dies with the ssh
// session — silently, and only for users who close their laptop.
func TestDetachedScriptSurvivesDisconnect(t *testing.T) {
	s := buildDetachedRunScript("/workspace", []string{"python", "train.py"})

	if !strings.Contains(s, "nohup") {
		t.Fatal("without nohup the run takes SIGHUP when sshd tears down the session")
	}
	if !strings.Contains(s, "< /dev/null") {
		t.Fatal("stdin must come off /dev/null or ssh will not return")
	}
	if !strings.Contains(s, `> "$d/log" 2>&1`) {
		t.Fatal("stdout+stderr must go to the log file, or ssh holds the connection open")
	}
	if !strings.Contains(s, `echo $? > "$d/status"`) {
		t.Fatal("the exit code must be recorded — a log that stops is otherwise ambiguous")
	}
}

// TestDetachedScriptDoesNotReExpandTheCommand: the user's command is written
// through a QUOTED heredoc, so the remote shell must not expand it on the way
// in. If it did, `echo $HOME` would be resolved by the wrong shell at the wrong
// time, and a command containing backticks would execute during upload.
func TestDetachedScriptDoesNotReExpandTheCommand(t *testing.T) {
	s := buildDetachedRunScript("/workspace", []string{"echo", "$HOME", "&&", "python", "-c", "print(1)"})

	if !strings.Contains(s, "<<'AQ_CMD_EOF'") {
		t.Fatal("the command heredoc must be quoted so the command is stored literally")
	}
	if !strings.Contains(s, "<<'AQ_RUN_EOF'") {
		t.Fatal("the runner heredoc must be quoted too")
	}
	if !strings.Contains(s, "echo $HOME && python -c print(1)") {
		t.Fatalf("the command must appear verbatim, got:\n%s", s)
	}
}

// TestDetachedScriptQuotesTheWorkdir: a path with a space is ordinary on a
// laptop and must not split into two arguments on the box.
func TestDetachedScriptQuotesTheWorkdir(t *testing.T) {
	s := buildDetachedRunScript("/work space/proj", []string{"ls"})
	if !strings.Contains(s, `'/work space/proj'`) {
		t.Fatalf("the workdir must be shell-quoted, got:\n%s", s)
	}
}

func TestLaunchDetachedReturnsTheRunID(t *testing.T) {
	got, err := launchDetached("aq-box", "/workspace", []string{"python", "train.py"},
		func(args []string) ([]byte, error) { return []byte("20260826-141230\n"), nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "20260826-141230" {
		t.Fatalf("got %q", got)
	}
}

// TestLaunchDetachedRefusesSilentSuccess: reporting a started run we cannot
// name would leave the user with no handle to read it back with.
func TestLaunchDetachedRefusesSilentSuccess(t *testing.T) {
	if _, err := launchDetached("aq-box", "/workspace", []string{"x"},
		func([]string) ([]byte, error) { return []byte("  \n"), nil }); err == nil {
		t.Fatal("want an error when the box reports no run id")
	}
	if _, err := launchDetached("aq-box", "/workspace", []string{"x"},
		func([]string) ([]byte, error) { return nil, errors.New("boom") }); err == nil {
		t.Fatal("want an error when ssh fails")
	}
}

// TestRunDetachPushesThenLaunchesWithoutHandingOffTheTerminal is the whole
// point: --detach must return, not block on ssh.
func TestRunDetachPushesThenLaunchesWithoutHandingOffTheTerminal(t *testing.T) {
	var out strings.Builder
	pushes, handoffs := 0, 0
	var gotWorkdir string

	err := runRun(runOptions{
		command:      []string{"python", "train.py"},
		detach:       true,
		push:         pushOptions{to: "/workspace"},
		out:          &out,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		doPush:       func(string, pushOptions) error { pushes++; return nil },
		handoff:      func([]string) error { handoffs++; return nil },
		launch: func(alias, workdir string, command []string) (string, error) {
			gotWorkdir = workdir
			return "20260826-141230", nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pushes != 1 {
		t.Fatalf("want the code sent first, got %d pushes", pushes)
	}
	if handoffs != 0 {
		t.Fatalf("--detach must not hand off the terminal, got %d", handoffs)
	}
	if gotWorkdir != "/workspace" {
		t.Fatalf("want the run launched in the push destination, got %q", gotWorkdir)
	}
	// The id alone on stdout is what makes `RUN=$(aq run --detach -- …)` work.
	if strings.TrimSpace(out.String()) != "20260826-141230" {
		t.Fatalf("stdout must carry only the run id, got %q", out.String())
	}
}
