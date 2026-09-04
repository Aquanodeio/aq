package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/config"
)

// fullSurvey is what a healthy box's survey script prints.
const fullSurvey = "uname=Linux\narch=x86_64\nogre_path=/usr/local/bin/ogre\nogre_version=ogre v0.1.35\nmount=yes\ngpu=NVIDIA H100 80GB HBM3\nok=1\n"

// stubRunner answers each remote script from a table keyed by a substring of
// the script, so a test states what the box says without caring how the script
// is assembled.
func stubRunner(t *testing.T, answers map[string]string) (remoteRunner, *[]string) {
	t.Helper()
	var seen []string
	return func(_ config.Host, remote string) ([]byte, error) {
		seen = append(seen, remote)
		for key, answer := range answers {
			if strings.Contains(remote, key) {
				if strings.HasPrefix(answer, "!ERR:") {
					return nil, errors.New(strings.TrimPrefix(answer, "!ERR:"))
				}
				return []byte(answer), nil
			}
		}
		t.Fatalf("unexpected remote command:\n%s", remote)
		return nil, nil
	}, &seen
}

func noSync(_ []config.Host) error { return nil }

func TestValidHostAliasRejectsWhatWouldNeedQuoting(t *testing.T) {
	for _, bad := range []string{"", "-lead", "has space", "Upper", "semi;colon", "quote'", "$var", strings.Repeat("a", 41)} {
		if err := validHostAlias(bad); err == nil {
			t.Errorf("validHostAlias(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"a", "lease-a", "box_1", "8xh100"} {
		if err := validHostAlias(good); err != nil {
			t.Errorf("validHostAlias(%q) = %v, want nil", good, err)
		}
	}
}

func TestParseSSHTarget(t *testing.T) {
	cases := []struct {
		in       string
		override string
		target   string
		port     int
		defRoot  bool
		bad      bool
	}{
		{in: "root@1.2.3.4", target: "root@1.2.3.4"},
		{in: "ubuntu@box.example.com:2222", target: "ubuntu@box.example.com", port: 2222},
		{in: "1.2.3.4", target: "root@1.2.3.4", defRoot: true},
		{in: "root@[2001:db8::1]:22", target: "root@2001:db8::1", port: 22},
		{in: "root@[2001:db8::1]", target: "root@2001:db8::1"},
		{in: "root@box:notaport", bad: true},
		{in: "", bad: true},
		// --ssh-user fills in a bare host and is not a "defaulted" guess.
		{in: "1.2.3.4", override: "ubuntu", target: "ubuntu@1.2.3.4"},
		// --ssh-user agreeing with an embedded user is a no-op, not a conflict.
		{in: "ubuntu@1.2.3.4", override: "ubuntu", target: "ubuntu@1.2.3.4"},
		// --ssh-user disagreeing with an embedded user is refused, not silently
		// resolved either way.
		{in: "root@1.2.3.4", override: "ubuntu", bad: true},
	}
	for _, tc := range cases {
		target, port, defaultedRoot, err := parseSSHTarget(tc.in, tc.override)
		if tc.bad {
			if err == nil {
				t.Errorf("parseSSHTarget(%q, %q) = %q, %d; want an error", tc.in, tc.override, target, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSSHTarget(%q, %q): %v", tc.in, tc.override, err)
			continue
		}
		if target != tc.target || port != tc.port {
			t.Errorf("parseSSHTarget(%q, %q) = %q, %d; want %q, %d", tc.in, tc.override, target, port, tc.target, tc.port)
		}
		if defaultedRoot != tc.defRoot {
			t.Errorf("parseSSHTarget(%q, %q) defaultedRoot = %v; want %v", tc.in, tc.override, defaultedRoot, tc.defRoot)
		}
	}
}

func TestRootLoginHint(t *testing.T) {
	// An explicit user (not defaulted) never gets aq's guess-hint: the caller
	// already made a deliberate choice, so a login failure there is a plain
	// SSH error, not "maybe you meant a different user".
	if hint := rootLoginHint(false, []byte("Please login as the user \"ubuntu\""), errors.New("exit status 1")); hint != "" {
		t.Errorf("rootLoginHint(defaultedRoot=false, ...) = %q, want no hint", hint)
	}
	if hint := rootLoginHint(true, []byte("Please login as the user \"ubuntu\" rather than the user \"root\"."), errors.New("exit status 1")); hint == "" {
		t.Error("rootLoginHint: expected a hint for the forced-command banner shape")
	}
	if hint := rootLoginHint(true, nil, errors.New("Permission denied (publickey).")); hint == "" {
		t.Error("rootLoginHint: expected a hint for a bare publickey rejection")
	}
	if hint := rootLoginHint(true, nil, errors.New("connection timed out")); hint != "" {
		t.Errorf("rootLoginHint: unrelated failure got a hint: %q", hint)
	}
}

// A survey that did not run to completion is a box we could not look at. It
// must not decode into a struct full of zero values that read as facts.
func TestParseSurveyRefusesAnIncompleteSurvey(t *testing.T) {
	if _, err := parseSurvey([]byte("uname=Linux\narch=x86_64\n")); err == nil {
		t.Fatal("expected a truncated survey to be refused")
	}
	s, err := parseSurvey([]byte(fullSurvey))
	if err != nil {
		t.Fatalf("parseSurvey: %v", err)
	}
	if s.OgrePath != "/usr/local/bin/ogre" || !s.MountExists || s.GPU == "" {
		t.Fatalf("survey decoded wrong: %+v", s)
	}
}

// --dry-run is the promise that a user can point aq at a machine before
// deciding anything. It must leave the registry, the ssh config, and the box
// exactly as it found them.
func TestHostAddDryRunWritesNothing(t *testing.T) {
	detachedSandbox(t)
	run, seen := stubRunner(t, map[string]string{
		"uname=":       fullSurvey,
		"ogre status":  `{"gpu":[]}`,
		"preflight_ok": fullSurvey,
	})

	var out bytes.Buffer
	err := runHost(hostOptions{
		sub: "add", alias: "lease-a", ssh: "root@1.2.3.4",
		dryRun: true, out: &out, errOut: &bytes.Buffer{},
		run: run,
		upload: func(config.Host, string, string) error {
			t.Fatal("dry run must not upload anything")
			return nil
		},
		syncConfig: func([]config.Host) error {
			t.Fatal("dry run must not touch the ssh config")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runHost add --dry-run: %v", err)
	}

	hosts, err := config.LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Fatalf("dry run recorded %d host(s)", len(hosts))
	}
	if path, err := managedHostsConfigPath(); err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatal("dry run wrote the ssh fragment")
		}
	}

	text := out.String()
	for _, want := range []string{"Would:", "Would NOT: contact the Aquanode API", "one box runs one setup", "--dry-run: nothing was installed"} {
		if !strings.Contains(strings.ToLower(text), strings.ToLower(want)) {
			t.Errorf("dry-run output is missing %q:\n%s", want, text)
		}
	}
	// Every remote command a dry run issues must be a read.
	for _, cmd := range *seen {
		for _, forbidden := range []string{"mv ", "chmod ", "nohup", "> "} {
			if strings.Contains(cmd, forbidden) {
				t.Errorf("dry run issued a writing command %q:\n%s", forbidden, cmd)
			}
		}
	}
}

// There is no public ogre installer. aq must refuse rather than invent a
// download, and the refusal has to say what to do instead.
func TestHostAddRefusesWhenTheBoxHasNoOgreAndNoBinaryWasGiven(t *testing.T) {
	detachedSandbox(t)
	survey := strings.Replace(fullSurvey, "ogre_path=/usr/local/bin/ogre", "ogre_path=", 1)
	run, _ := stubRunner(t, map[string]string{"uname=": survey})

	err := runHost(hostOptions{
		sub: "add", alias: "lease-a", ssh: "root@1.2.3.4",
		out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		run: run, syncConfig: noSync,
		upload: func(config.Host, string, string) error {
			t.Fatal("must not upload without --ogre-binary")
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "--ogre-binary") {
		t.Fatalf("expected a refusal naming --ogre-binary, got %v", err)
	}
	if hosts, _ := config.LoadHosts(); len(hosts) != 0 {
		t.Fatalf("a failed add recorded %d host(s)", len(hosts))
	}
}

// The daemon check is what makes the registry entry mean something. A box whose
// ogre does not answer on loopback must not be recorded as usable.
func TestHostAddRefusesWhenTheDaemonDoesNotAnswerOnLoopback(t *testing.T) {
	detachedSandbox(t)
	run, _ := stubRunner(t, map[string]string{
		"uname=":      fullSurvey,
		"ogre status": "!ERR:connection refused",
	})

	err := runHost(hostOptions{
		sub: "add", alias: "lease-a", ssh: "root@1.2.3.4",
		out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		run: run, syncConfig: noSync,
	})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("expected a refusal naming the loopback check, got %v", err)
	}
	if hosts, _ := config.LoadHosts(); len(hosts) != 0 {
		t.Fatalf("a failed add recorded %d host(s)", len(hosts))
	}
}

func TestHostAddRecordsTheBoxAndItsStanza(t *testing.T) {
	detachedSandbox(t)
	run, _ := stubRunner(t, map[string]string{
		"uname=":      fullSurvey,
		"ogre status": `{"gpu":[{"name":"H100"}]}`,
	})

	var out bytes.Buffer
	err := runHost(hostOptions{
		sub: "add", alias: "lease-a", ssh: "root@1.2.3.4:2222",
		mountPath: "/data", out: &out, errOut: &bytes.Buffer{},
		run: run,
	})
	if err != nil {
		t.Fatalf("runHost add: %v", err)
	}

	hosts, err := config.LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("expected 1 host, got %d", len(hosts))
	}
	h := hosts[0]
	if h.Alias != "lease-a" || h.SSH != "root@1.2.3.4" || h.Port != 2222 || h.MountPath != "/data" {
		t.Fatalf("recorded the wrong entry: %+v", h)
	}
	if h.Attached() {
		t.Fatal("`aq host add` must never mark a box attached — that is `aq attach`'s job, gated on a probe")
	}
	if h.AddedAt == "" {
		t.Error("AddedAt was not stamped")
	}
	if !strings.Contains(out.String(), "aq attach lease-a") {
		t.Errorf("the success message should point at attach:\n%s", out.String())
	}
}

func TestHostAddRefusesADuplicateAlias(t *testing.T) {
	detachedSandbox(t, testHost())
	err := runHost(hostOptions{
		sub: "add", alias: "lease-a", ssh: "root@9.9.9.9",
		out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
		run: func(config.Host, string) ([]byte, error) {
			t.Fatal("must not reach the box before noticing the duplicate")
			return nil, nil
		},
		syncConfig: noSync,
	})
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected a duplicate-alias refusal, got %v", err)
	}
}

// `aq host rm` forgets a machine. It must never be a way to act on one, and it
// must not silently orphan an attached box's deployment row.
func TestHostRmRefusesAnAttachedBoxAndNamesRelease(t *testing.T) {
	attached := testHost()
	attached.DeploymentID = 4242
	attached.AttachedAt = "2026-08-27T00:00:00Z"
	detachedSandbox(t, attached)

	err := runHost(hostOptions{sub: "rm", alias: "lease-a", out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, syncConfig: noSync})
	if err == nil || !strings.Contains(err.Error(), "aq release") {
		t.Fatalf("expected a refusal naming `aq release`, got %v", err)
	}
	if hosts, _ := config.LoadHosts(); len(hosts) != 1 {
		t.Fatal("the host was removed despite the refusal")
	}
}

func TestHostRmForgetsADetachedBox(t *testing.T) {
	detachedSandbox(t, testHost())
	var out bytes.Buffer
	if err := runHost(hostOptions{sub: "rm", alias: "lease-a", out: &out, errOut: &bytes.Buffer{}, syncConfig: noSync}); err != nil {
		t.Fatalf("runHost rm: %v", err)
	}
	if hosts, _ := config.LoadHosts(); len(hosts) != 0 {
		t.Fatal("the host was not removed")
	}
	if !strings.Contains(out.String(), "still running") {
		t.Errorf("rm must say the box is untouched:\n%s", out.String())
	}
}

func TestHostLsIsEmptyAndQuietOnAFreshMachine(t *testing.T) {
	detachedSandbox(t)
	var out bytes.Buffer
	if err := runHost(hostOptions{sub: "ls", out: &out, errOut: &bytes.Buffer{}}); err != nil {
		t.Fatalf("runHost ls: %v", err)
	}
	if !strings.Contains(out.String(), "aq host add") {
		t.Errorf("an empty list should say how to add one:\n%s", out.String())
	}
}

func TestProbeOgreDaemonIsThreeState(t *testing.T) {
	h := testHost()

	state, _ := probeOgreDaemon(func(config.Host, string) ([]byte, error) { return []byte(`{"gpu":[]}`), nil }, h)
	if state != "ok" {
		t.Errorf("a JSON answer should be ok, got %q", state)
	}
	state, reason := probeOgreDaemon(func(config.Host, string) ([]byte, error) {
		return []byte("ogre status: dial tcp 127.0.0.1:3000: connection refused"), nil
	}, h)
	if state != "unreachable" || !strings.Contains(reason, "connection refused") {
		t.Errorf("a non-JSON answer should be unreachable with its reason, got %q / %q", state, reason)
	}
	state, reason = probeOgreDaemon(func(config.Host, string) ([]byte, error) {
		return nil, errors.New("ssh: handshake failed")
	}, h)
	if state != "unknown" {
		t.Errorf("a failed check is unknown, never ok; got %q / %q", state, reason)
	}
}
