package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// A subagent doing an unrelated rename smoke-tested the built binary from a
// scratch checkout and rented a real box in production. Nothing it did was
// wrong in isolation: it never set AQ_API_URL (so the default applied), it
// inherited the operator's stored credential (so it was authenticated), and
// `aq up` asks nobody anything. These tests pin the refusal that makes that
// specific sequence impossible, and the exemptions that keep a person at a
// terminal unaffected.

func TestGuardBillableRefusesRemoteHostWithoutATerminal(t *testing.T) {
	err := guardBillable("up", "https://server.aquanode.io/api/v1", nil, false, false)
	if err == nil {
		t.Fatal("aq up against production from a non-interactive shell must be refused")
	}
	msg := err.Error()
	// The message has to name the host. The whole failure mode is not knowing
	// which host you were on, so a refusal that withholds it teaches nothing.
	if !strings.Contains(msg, "server.aquanode.io") {
		t.Errorf("refusal must name the host it refused; got: %s", msg)
	}
	// ...and it has to name the way out, or the next person just deletes the guard.
	if !strings.Contains(msg, "--prod") || !strings.Contains(msg, "AQ_ALLOW_PROD=1") {
		t.Errorf("refusal must name both overrides; got: %s", msg)
	}
}

func TestGuardBillableAllowsALocalStack(t *testing.T) {
	for _, target := range []string{
		"http://localhost:4980/api/v1",
		"http://127.0.0.1:4980/api/v1",
		"http://[::1]:4980/api/v1",
		"http://slot19.localhost:4980/api/v1",
	} {
		if err := guardBillable("up", target, nil, false, false); err != nil {
			t.Errorf("%s is a local stack and must not be guarded: %v", target, err)
		}
	}
}

// The point of the worktree wiring on the harness side: a binary run inside a
// worktree resolves that worktree's own orchestrator, and this is the assertion
// that such a target sails through the guard with no opt-in at all.
func TestGuardBillableIsSilentForEveryVerbThatCannotRentHardware(t *testing.T) {
	// down and pause STOP spend. Guarding them would make the cheap action
	// harder than the expensive one, which is the wrong way round.
	for _, cmd := range []string{"down", "pause", "ls", "status", "logs", "save", "share", "fork", "push", "run", "ssh", "whoami"} {
		if err := guardBillable(cmd, "https://server.aquanode.io/api/v1", nil, false, false); err != nil {
			t.Errorf("aq %s rents nothing and must not be guarded: %v", cmd, err)
		}
	}
}

func TestGuardBillableCoversEveryBillableVerb(t *testing.T) {
	for _, cmd := range []string{"up", "deploy", "import"} {
		if err := guardBillable(cmd, "https://server.aquanode.io/api/v1", nil, false, false); err == nil {
			t.Errorf("aq %s can lease hardware and must be guarded", cmd)
		}
	}
}

func TestGuardBillableExemptsATerminalAndAnExplicitOptIn(t *testing.T) {
	prod := "https://server.aquanode.io/api/v1"
	if err := guardBillable("up", prod, nil, false, true); err != nil {
		t.Errorf("a person at a terminal must not be blocked: %v", err)
	}
	if err := guardBillable("up", prod, nil, true, false); err != nil {
		t.Errorf("an explicit opt-in must be honored: %v", err)
	}
}

// An unparseable or unfamiliar host is guarded exactly like production. The
// accident is never "I meant a different remote host", it is not knowing the
// target was remote at all — so "could not tell" has to block.
func TestIsLocalTargetIsDefaultDeny(t *testing.T) {
	for _, target := range []string{
		"https://server.aquanode.io/api/v1",
		"http://staging.internal/api/v1",
		"http://10.0.0.5:4980/api/v1",
		"://not a url",
		"",
	} {
		if isLocalTarget(target) {
			t.Errorf("%q is not provably loopback and must count as remote", target)
		}
	}
}

func TestStripProdFlagLeavesARemoteCommandsArgvAlone(t *testing.T) {
	// `aq run <box> -- python train.py --prod` forwards everything past the
	// separator to the box. Eating a flag out of it would silently change what
	// the user asked the box to do.
	args := []string{"mybox", "--prod", "--", "python", "train.py", "--prod"}
	got, found := stripProdFlag(args)
	if !found {
		t.Fatal("the --prod before the separator is aq's own and must be consumed")
	}
	want := []string{"mybox", "--", "python", "train.py", "--prod"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv past `--` must survive verbatim\n got: %v\nwant: %v", got, want)
	}
}

func TestStripProdFlagReportsAbsence(t *testing.T) {
	got, found := stripProdFlag([]string{"--gpus", "1"})
	if found {
		t.Error("no --prod was passed; the opt-in must not be inferred")
	}
	if strings.Join(got, " ") != "--gpus 1" {
		t.Errorf("unrelated args must survive: %v", got)
	}
}

func TestAnnounceTargetNamesTheHostForAnythingThatChangesState(t *testing.T) {
	var buf bytes.Buffer
	announceTarget("up", "https://server.aquanode.io/api/v1", &buf)
	if !strings.Contains(buf.String(), "https://server.aquanode.io/api/v1") {
		t.Errorf("a state-changing command must print its resolved host; got %q", buf.String())
	}
}

func TestAnnounceTargetStaysQuietForReads(t *testing.T) {
	for _, cmd := range []string{"ls", "status", "logs", "gpus", "whoami", "version", "help", "host"} {
		var buf bytes.Buffer
		announceTarget(cmd, "https://server.aquanode.io/api/v1", &buf)
		if buf.Len() != 0 {
			t.Errorf("aq %s changes nothing and must not print a host banner; got %q", cmd, buf.String())
		}
	}
}

// The guard resolves its host through the same precedence every command uses,
// so pointing a worktree at its own stack is enough to disarm it — no flag, no
// env opt-in. This is the end-to-end shape of the harness-side fix.
func TestAPIURLOverrideDisarmsTheGuard(t *testing.T) {
	t.Setenv("AQ_API_URL", "http://localhost:4980/api/v1")
	cred := credentialPointingAtProd()
	if err := guardBillable("up", resolveAPIURL(cred), nil, false, false); err != nil {
		t.Fatalf("a worktree pointed at its own orchestrator must be able to run aq up: %v", err)
	}
}

// ...and with the override unset, a stored production credential is exactly
// what puts an unsuspecting non-interactive run in front of the refusal.
func TestAStoredProdCredentialAloneTripsTheGuard(t *testing.T) {
	t.Setenv("AQ_API_URL", "")
	cred := credentialPointingAtProd()
	if err := guardBillable("up", resolveAPIURL(cred), nil, false, false); err == nil {
		t.Fatal("an inherited production credential with no override must be refused")
	}
}

func TestIsInteractiveStdinIsFalseUnderGoTest(t *testing.T) {
	// The rail depends on this being false for every automated caller. `go
	// test` runs with stdin on /dev/null or a pipe, which is the same shape a
	// script or an agent has — if this ever reports true the guard is off for
	// exactly the callers it exists for.
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		t.Skip("stdin is a terminal here; the guard's non-interactive path is not exercised")
	}
	if isInteractiveStdin() {
		t.Error("stdin is not a character device, so isInteractiveStdin must report false")
	}
}

func credentialPointingAtProd() *config.Credential {
	return &config.Credential{APIURL: "https://server.aquanode.io/api/v1", Token: "t"}
}

// `aq up --help` describes a command; it rents nothing. A guard that refuses to
// answer that question is one people learn to disable.
func TestGuardBillableNeverBlocksHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--provider", "akash", "--help"}} {
		if err := guardBillable("up", "https://server.aquanode.io/api/v1", args, false, false); err != nil {
			t.Errorf("aq up %v asks for usage and must not be refused: %v", args, err)
		}
	}
	// ...but a --help meant for a command running on the box is not aq's.
	if err := guardBillable("up", "https://server.aquanode.io/api/v1", []string{"--", "trainer", "--help"}, false, false); err == nil {
		t.Error("a --help past the separator belongs to the remote command and must not disarm the guard")
	}
}

// An automated tool launched from an interactive shell inherits that shell's
// terminal, so the TTY test alone calls it a person. That is the caller from
// the incident, so the environment signal has to override the terminal.
func TestHasHumanOversightIgnoresAnInheritedTerminal(t *testing.T) {
	for _, key := range []string{"CI", "CONTINUOUS_INTEGRATION", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT"} {
		env := func(k string) string {
			if k == key {
				return "1"
			}
			return ""
		}
		if hasHumanOversight(env, true) {
			t.Errorf("%s announces automation; an inherited terminal must not count as oversight", key)
		}
	}
	none := func(string) string { return "" }
	if !hasHumanOversight(none, true) {
		t.Error("a real terminal with no automation markers is oversight")
	}
	if hasHumanOversight(none, false) {
		t.Error("no terminal and no markers is not oversight")
	}
}

// /dev/null is a character device. The old character-device-only test called
// `aq up </dev/null` interactive — the exact shape every script runs in.
func TestIsInteractiveStdinRejectsDevNull(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	saved := os.Stdin
	os.Stdin = devNull
	defer func() { os.Stdin = saved }()

	if isInteractiveStdin() {
		t.Error("stdin redirected from /dev/null is not a person, however much it looks like a character device")
	}
}
