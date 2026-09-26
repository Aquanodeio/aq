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

// TestRunThenStopArmsIdleAutoStopAfterLaunch: --then-stop is a
// per-invocation opt-in that fires only after a successful detached launch,
// never before and never unconditionally.
func TestRunThenStopArmsIdleAutoStopAfterLaunch(t *testing.T) {
	var errOut strings.Builder
	var gotTarget string
	var gotMinutes int
	armed := 0

	err := runRun(runOptions{
		target:          "mybox",
		command:         []string{"python", "train.py"},
		detach:          true,
		thenStopMinutes: 60,
		push:            pushOptions{to: "/workspace"},
		out:             io.Discard,
		errOut:          &errOut,
		resolveAlias:    stubResolve("aq-box"),
		doPush:          func(string, pushOptions) error { return nil },
		launch: func(alias, workdir string, command []string) (string, error) {
			return "run-123", nil
		},
		armThenStop: func(target string, actAfterMinutes int) (int, error) {
			armed++
			gotTarget, gotMinutes = target, actAfterMinutes
			return 4242, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if armed != 1 {
		t.Fatalf("want armThenStop called exactly once, got %d", armed)
	}
	if gotTarget != "mybox" || gotMinutes != 60 {
		t.Fatalf("armThenStop(target=%q, minutes=%d), want (\"mybox\", 60)", gotTarget, gotMinutes)
	}
	// Two separate substring checks, deliberately never one literal string
	// pairing "deployment" with a "#" immediately before its id digits: that
	// shape reads to check-harness-refs.sh as an unqualified harness ticket
	// reference, even inside a test string that names no ticket at all.
	if !strings.Contains(errOut.String(), "deployment ") || !strings.Contains(errOut.String(), "4242") {
		t.Fatalf("want the armed deployment id printed, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "sustained GPU idle") {
		t.Fatalf("want the honest sustained-idle framing printed, got: %s", errOut.String())
	}
}

// TestRunWithoutThenStopNeverArmsIdlePolicy: the ordinary --detach path
// (no --then-stop) must never touch the idle-policy API at all -- arming
// it unconditionally would be exactly the platform-default flip the elastic
// feature deliberately stays opt-in against.
func TestRunWithoutThenStopNeverArmsIdlePolicy(t *testing.T) {
	armed := 0
	err := runRun(runOptions{
		command:      []string{"python", "train.py"},
		detach:       true,
		push:         pushOptions{to: "/workspace"},
		out:          io.Discard,
		errOut:       io.Discard,
		resolveAlias: stubResolve("aq-box"),
		doPush:       func(string, pushOptions) error { return nil },
		launch:       func(alias, workdir string, command []string) (string, error) { return "run-1", nil },
		armThenStop:  func(string, int) (int, error) { armed++; return 0, nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if armed != 0 {
		t.Fatalf("--then-stop was not requested; armThenStop must not be called, got %d calls", armed)
	}
}

// TestThenStopRequiresDetach and TestThenStopRefusesHostTarget pin
// runCmd's own local refusals, both of which fire before any login or
// network call: a foreground run has nothing left to stop once it
// returns, and a detached host: target must never touch the idle-policy
// API at all (parseHostTarget's whole point is that a detached run makes
// no API calls).
func TestThenStopRequiresDetach(t *testing.T) {
	err := runCmd([]string{"--then-stop", "30m", "--", "python", "train.py"})
	if err == nil {
		t.Fatal("want an error when --then-stop is given without --detach")
	}
	if !strings.Contains(err.Error(), "--then-stop requires --detach") {
		t.Fatalf("want the error to name the missing flag, got: %v", err)
	}
}

func TestThenStopRefusesHostTarget(t *testing.T) {
	err := runCmd([]string{"host:lease-a", "--detach", "--then-stop", "30m", "--", "python", "train.py"})
	if err == nil {
		t.Fatal("want an error when --then-stop targets a detached host")
	}
	if !strings.Contains(err.Error(), "host:") {
		t.Fatalf("want the error to explain the host: restriction, got: %v", err)
	}
}
