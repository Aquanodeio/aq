package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// detachedSandbox points aq's config dir and $HOME at scratch directories and
// seeds the host registry.
//
// $HOME is enough here and only here: aq resolves its OWN paths from $HOME, so
// every file these tests cause aq to write lands in the temp tree. It would NOT
// be enough for a test that execs a real ssh — OpenSSH resolves `~` from the
// passwd entry, not the environment, so it would still read the operator's real
// ~/.ssh/config. That is precisely why every test below injects the ssh handoff
// instead of running one.
func detachedSandbox(t *testing.T, hosts ...config.Host) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AQ_CONFIG_DIR", filepath.Join(home, "aqconfig"))
	if err := config.SaveHosts(hosts); err != nil {
		t.Fatalf("seed hosts: %v", err)
	}
}

func testHost() config.Host {
	return config.Host{
		Alias:     "lease-a",
		SSH:       "root@1.2.3.4",
		Identity:  "/keys/id_ed25519",
		MountPath: "/workspace",
		AddedAt:   "2026-08-27T00:00:00Z",
	}
}

// TestDetachedVerbsNeverTouchTheAPI is the rail for the whole detached mode.
//
// Every verb runs with a genuinely nil *api.Client. That is not a stylistic
// choice and it must not be softened into a mock that records calls: with a nil
// client, any code path that decides to "just check" something server-side is a
// nil-pointer panic that fails this test loudly, where a recording mock would
// let a silent fallback to DefaultAPIURL pass review and only surface later in
// somebody's access log. The nil IS the assertion.
func TestDetachedVerbsNeverTouchTheAPI(t *testing.T) {
	var nilClient *api.Client

	t.Run("ssh", func(t *testing.T) {
		detachedSandbox(t, testHost())
		var got []string
		err := runSSH(sshOptions{
			cred:    nil, // no login at all — a detached box needs none
			target:  "host:lease-a",
			out:     &bytes.Buffer{},
			errOut:  &bytes.Buffer{},
			handoff: func(args []string) error { got = args; return nil },
		})
		if err != nil {
			t.Fatalf("runSSH: %v", err)
		}
		if len(got) == 0 || got[0] != hostAliasFor("lease-a") {
			t.Fatalf("ssh did not target the host alias: %v", got)
		}
	})

	t.Run("push", func(t *testing.T) {
		detachedSandbox(t, testHost())
		local := t.TempDir()
		var plan transferPlan
		err := runPush(pushOptions{
			cred:       nil,
			target:     "host:lease-a",
			from:       local,
			out:        &bytes.Buffer{},
			errOut:     &bytes.Buffer{},
			probeRsync: func(string) bool { return false },
			transfer:   func(p transferPlan, _ io.Writer) error { plan = p; return nil },
		})
		if err != nil {
			t.Fatalf("runPush: %v", err)
		}
		if plan.alias != hostAliasFor("lease-a") {
			t.Fatalf("push did not target the host alias: %q", plan.alias)
		}
	})

	t.Run("run", func(t *testing.T) {
		detachedSandbox(t, testHost())
		var got []string
		err := runRun(runOptions{
			cred:    nil,
			target:  "host:lease-a",
			command: []string{"nvidia-smi"},
			noPush:  true,
			out:     &bytes.Buffer{},
			errOut:  &bytes.Buffer{},
			handoff: func(args []string) error { got = args; return nil },
		})
		if err != nil {
			t.Fatalf("runRun: %v", err)
		}
		if len(got) == 0 || got[0] != hostAliasFor("lease-a") {
			t.Fatalf("run did not target the host alias: %v", got)
		}
	})

	t.Run("logs", func(t *testing.T) {
		detachedSandbox(t, testHost())
		var got []string
		err := runLogs(logsOptions{
			cred:    nil,
			target:  "host:lease-a",
			lines:   10,
			out:     &bytes.Buffer{},
			errOut:  &bytes.Buffer{},
			handoff: func(args []string) error { got = args; return nil },
		})
		if err != nil {
			t.Fatalf("runLogs: %v", err)
		}
		if len(got) == 0 || got[0] != hostAliasFor("lease-a") {
			t.Fatalf("logs did not target the host alias: %v", got)
		}
	})

	for _, tc := range []struct {
		verb string
		want string
	}{
		{"status", "ogre status"},
		{"save", "ogre snapshot"},
		{"sync-now", "ogre push"},
		{"up", "ogre up"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			detachedSandbox(t, testHost())
			var got []string
			err := runDetached(detachedOptions{
				client:  nilClient,
				verb:    tc.verb,
				alias:   "lease-a",
				out:     &bytes.Buffer{},
				errOut:  &bytes.Buffer{},
				handoff: func(args []string) error { got = args; return nil },
			})
			if err != nil {
				t.Fatalf("runDetached(%s): %v", tc.verb, err)
			}
			joined := strings.Join(got, " ")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("%s should drive %q on the box; got %q", tc.verb, tc.want, joined)
			}
			if got[0] != hostAliasFor("lease-a") {
				t.Fatalf("%s did not target the host alias: %v", tc.verb, got)
			}
		})
	}
}

// A verb with no detached equivalent must say so, not silently do something
// close enough on a box the user leases.
func TestRunDetachedRefusesAVerbTheControlPlaneOwns(t *testing.T) {
	detachedSandbox(t, testHost())
	err := runDetached(detachedOptions{
		verb:    "share",
		alias:   "lease-a",
		out:     &bytes.Buffer{},
		errOut:  &bytes.Buffer{},
		handoff: func([]string) error { t.Fatal("must not exec"); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "control plane") {
		t.Fatalf("expected a refusal naming the control plane, got %v", err)
	}
}

func TestParseHostTargetOnlyMatchesThePrefix(t *testing.T) {
	cases := []struct {
		in    string
		alias string
		ok    bool
	}{
		{"host:lease-a", "lease-a", true},
		{"host: lease-a ", "lease-a", true},
		{"host:", "", false},
		{"lease-a", "", false},
		{"", "", false},
		{"4242", "", false},
		{"hosts:lease-a", "", false},
	}
	for _, tc := range cases {
		alias, ok := parseHostTarget(tc.in)
		if ok != tc.ok || alias != tc.alias {
			t.Errorf("parseHostTarget(%q) = %q, %v; want %q, %v", tc.in, alias, ok, tc.alias, tc.ok)
		}
	}
}

// An unregistered alias must name the fix, not fail with a lookup error the
// user cannot act on.
func TestResolveHostSSHAliasNamesTheFixForAnUnknownAlias(t *testing.T) {
	detachedSandbox(t)
	_, err := resolveHostSSHAlias("nope")
	if err == nil || !strings.Contains(err.Error(), "aq host add") {
		t.Fatalf("expected an error pointing at `aq host add`, got %v", err)
	}
}

// resolveSSHAlias must refuse rather than dereference a nil client when the
// target is NOT a detached host. Without this the nil-client rail would turn a
// missing login into a panic instead of a message.
func TestResolveSSHAliasRefusesANilClientForAManagedTarget(t *testing.T) {
	detachedSandbox(t)
	_, err := resolveSSHAlias(nil, "4242", "ssh", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "aq login") {
		t.Fatalf("expected an error pointing at `aq login`, got %v", err)
	}
}

// The generated host stanza is the thing ssh actually uses. It must carry the
// registered login, port and absolute key path — a tilde'd IdentityFile is the
// documented way to end up pointing at a key that is not there.
func TestSyncHostConfigWritesAStanzaPerRegisteredBox(t *testing.T) {
	detachedSandbox(t)
	hosts := []config.Host{
		{Alias: "lease-a", SSH: "ubuntu@1.2.3.4", Port: 2222, Identity: "/keys/id_ed25519"},
	}
	if err := syncHostConfig(hosts); err != nil {
		t.Fatalf("syncHostConfig: %v", err)
	}

	path, err := managedHostsConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := readFileString(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Host " + hostAliasFor("lease-a"),
		"HostName 1.2.3.4",
		"Port 2222",
		"User ubuntu",
		"IdentityFile /keys/id_ed25519",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("host fragment is missing %q:\n%s", want, data)
		}
	}
	if strings.Contains(data, "~/") {
		t.Errorf("generated stanza must use absolute paths, got:\n%s", data)
	}
}

// The two fragments must not share a file: one is regenerated from the API's
// deployment list, the other from the local registry, and a detached run cannot
// make the API call the first one needs.
func TestBothFragmentsAreIncludedFromOneMarkerRegion(t *testing.T) {
	detachedSandbox(t)
	if err := syncHostConfig([]config.Host{{Alias: "a", SSH: "root@1.1.1.1"}}); err != nil {
		t.Fatalf("syncHostConfig: %v", err)
	}
	path, err := userConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := readFileString(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(data, beginMarker) != 1 || strings.Count(data, endMarker) != 1 {
		t.Fatalf("expected exactly one marker region, got:\n%s", data)
	}
	if !strings.Contains(data, managedConfigName) || !strings.Contains(data, managedHostsConfigName) {
		t.Fatalf("both fragments must be included:\n%s", data)
	}
}

// readFileString is a tiny helper so the assertions above read as assertions.
func readFileString(path string) (string, error) {
	data, err := os.ReadFile(path)
	return string(data), err
}

// A box registered with a non-default workspace root must actually receive its
// files there. Landing a push in /workspace on a box whose data volume is
// /data is silent: the transfer succeeds, and the `aq run` after it executes in
// a directory nothing was written to.
func TestDetachedPushAndRunUseTheRegisteredMountPath(t *testing.T) {
	h := testHost()
	h.MountPath = "/data"

	t.Run("resolved destination", func(t *testing.T) {
		detachedSandbox(t, h)
		if got := hostMountPathFor("host:lease-a"); got != "/data" {
			t.Fatalf("hostMountPathFor = %q, want /data", got)
		}
		if got := hostMountPathFor("4242"); got != "" {
			t.Fatalf("a marketplace target must resolve no host mount path, got %q", got)
		}
		if got := hostMountPathFor("host:unknown"); got != "" {
			t.Fatalf("an unregistered alias must resolve no mount path, got %q", got)
		}

		local := t.TempDir()
		var plan transferPlan
		err := runPush(pushOptions{
			cred:       nil,
			target:     "host:lease-a",
			from:       local,
			to:         hostMountPathFor("host:lease-a"),
			out:        &bytes.Buffer{},
			errOut:     &bytes.Buffer{},
			probeRsync: func(string) bool { return false },
			transfer:   func(p transferPlan, _ io.Writer) error { plan = p; return nil },
		})
		if err != nil {
			t.Fatalf("runPush: %v", err)
		}
		if plan.to != "/data" {
			t.Fatalf("push destination = %q, want the registered /data", plan.to)
		}
	})
}
