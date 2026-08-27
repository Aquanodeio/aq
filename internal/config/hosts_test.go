package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The host registry is the only state a detached run has. These cover the
// properties that make it safe to keep beside a credential file: it is 0600, a
// missing file is an empty registry rather than an error, and put/remove touch
// exactly one entry.

func TestLoadHostsOnAFreshMachineIsEmptyNotAnError(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", filepath.Join(t.TempDir(), "aq"))

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatalf("LoadHosts on a fresh machine: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("expected no hosts, got %d", len(hosts))
	}
}

func TestSaveHostsWrites0600(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "aq")
	t.Setenv("AQ_CONFIG_DIR", dir)

	if err := SaveHosts([]Host{{Alias: "lease-a", SSH: "root@1.2.3.4"}}); err != nil {
		t.Fatalf("SaveHosts: %v", err)
	}

	path, err := HostsPath()
	if err != nil {
		t.Fatalf("HostsPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("hosts.json is %#o, want 0600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir is %#o, want 0700", perm)
	}
}

// A loosely-permissioned registry written by an older aq (or another tool) must
// be tightened on the next write, not left as it was — the same repair Save
// applies to credentials.json.
func TestSaveHostsTightensAnExistingLooseFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "aq")
	t.Setenv("AQ_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, hostsFileName)
	if err := os.WriteFile(path, []byte(`{"hosts":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SaveHosts([]Host{{Alias: "a", SSH: "root@1.1.1.1"}}); err != nil {
		t.Fatalf("SaveHosts: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("hosts.json left at %#o, want 0600", perm)
	}
}

func TestPutHostReplacesOneEntryAndLeavesTheRest(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", filepath.Join(t.TempDir(), "aq"))
	if err := SaveHosts([]Host{
		{Alias: "a", SSH: "root@1.1.1.1"},
		{Alias: "b", SSH: "root@2.2.2.2", MountPath: "/data"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := PutHost(Host{Alias: "a", SSH: "root@9.9.9.9"}); err != nil {
		t.Fatalf("PutHost: %v", err)
	}

	hosts, err := LoadHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("expected 2 hosts, got %d", len(hosts))
	}
	if hosts[0].SSH != "root@9.9.9.9" {
		t.Errorf("a was not replaced: %+v", hosts[0])
	}
	if hosts[1].MountPath != "/data" {
		t.Errorf("b was disturbed: %+v", hosts[1])
	}
}

func TestRemoveHostReportsWhetherItWasThere(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", filepath.Join(t.TempDir(), "aq"))
	if err := SaveHosts([]Host{{Alias: "a", SSH: "root@1.1.1.1"}}); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveHost("a")
	if err != nil || !removed {
		t.Fatalf("RemoveHost(a) = %v, %v; want true, nil", removed, err)
	}
	removed, err = RemoveHost("a")
	if err != nil || removed {
		t.Fatalf("second RemoveHost(a) = %v, %v; want false, nil", removed, err)
	}
}

// Attached() is what every "is this box in the control plane" decision reads.
// A deployment id with no attach stamp is a box mid-attach or one whose probe
// failed, and it must not read as attached.
func TestAttachedRequiresBothTheIDAndTheStamp(t *testing.T) {
	cases := []struct {
		name string
		host Host
		want bool
	}{
		{"detached", Host{Alias: "a"}, false},
		{"id but no stamp", Host{Alias: "a", DeploymentID: 7}, false},
		{"stamp but no id", Host{Alias: "a", AttachedAt: "2026-08-27T00:00:00Z"}, false},
		{"both", Host{Alias: "a", DeploymentID: 7, AttachedAt: "2026-08-27T00:00:00Z"}, true},
	}
	for _, tc := range cases {
		if got := tc.host.Attached(); got != tc.want {
			t.Errorf("%s: Attached() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
