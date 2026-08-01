package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
)

// TestSSHEndpointForPrefersServiceURLs is the load-bearing provider case:
// simplepod silently publishes the ogre agent's HTTP port as app_url when the
// box maps no SSH port, so an aq that trusted app_url would dial an HTTP server
// and hang with nothing in the error hinting why. service_urls is keyed by the
// box's internal port, so its port-22 entry is unambiguously sshd.
func TestSSHEndpointForPrefersServiceURLs(t *testing.T) {
	cases := []struct {
		name        string
		serviceURLs string
		appURL      string
		wantHost    string
		wantPort    string
		wantOK      bool
	}{
		{
			name:        "simplepod: app_url is the ogre port, service_urls has the real sshd",
			serviceURLs: `[{"url":"http://198.51.100.4:3000","port":3000},{"url":"http://198.51.100.4:41022","port":22}]`,
			appURL:      "http://198.51.100.4:3000",
			wantHost:    "198.51.100.4", wantPort: "41022", wantOK: true,
		},
		{
			name:        "no port-22 entry falls back to app_url",
			serviceURLs: `[{"url":"http://198.51.100.4:3000","port":3000}]`,
			appURL:      "http://203.0.113.9:22",
			wantHost:    "203.0.113.9", wantPort: "22", wantOK: true,
		},
		{
			name:        "port arriving as a string still matches",
			serviceURLs: `[{"url":"http://198.51.100.4:2222","port":"22"}]`,
			wantHost:    "198.51.100.4", wantPort: "2222", wantOK: true,
		},
		{
			name:        "malformed service_urls must not break the fallback",
			serviceURLs: `{"unexpected":"shape"}`,
			appURL:      "http://203.0.113.9:22",
			wantHost:    "203.0.113.9", wantPort: "22", wantOK: true,
		},
		{
			name:        "akash with no SSH mapping resolves to nothing",
			serviceURLs: `[]`,
			appURL:      "",
			wantOK:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dep := api.Deployment{ID: 1, AppURL: c.appURL, ServiceURLs: json.RawMessage(c.serviceURLs)}
			host, port, ok := sshEndpointFor(dep)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (host != c.wantHost || port != c.wantPort) {
				t.Errorf("got %s:%s, want %s:%s", host, port, c.wantHost, c.wantPort)
			}
		})
	}
}

func TestBuildSSHArgs(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		forwards []string
		remote   []string
		want     []string
	}{
		{name: "bare", want: []string{"aq-box"}},
		{name: "remote command", remote: []string{"nvidia-smi", "-L"}, want: []string{"aq-box", "nvidia-smi", "-L"}},
		{name: "user override", user: "ubuntu", want: []string{"-l", "ubuntu", "aq-box"}},
		{
			name:     "repeatable forwards come before the target",
			forwards: []string{"8888:localhost:8888", "6006:localhost:6006"},
			want:     []string{"-L", "8888:localhost:8888", "-L", "6006:localhost:6006", "aq-box"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildSSHArgs("aq-box", c.user, c.forwards, c.remote)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("buildSSHArgs = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSplitRemoteCommand(t *testing.T) {
	head, remote := splitRemoteCommand([]string{"box", "--print", "--", "nvidia-smi", "--list-gpus"})
	if strings.Join(head, " ") != "box --print" {
		t.Errorf("head = %v", head)
	}
	if strings.Join(remote, " ") != "nvidia-smi --list-gpus" {
		t.Errorf("remote = %v", remote)
	}

	// `--print` is a flag, not the terminator.
	head, remote = splitRemoteCommand([]string{"--print", "box"})
	if strings.Join(head, " ") != "--print box" || remote != nil {
		t.Errorf("head = %v, remote = %v", head, remote)
	}
}

// TestSSHCmdRejectsFlagLikeTarget guards the same class of problem
// validateBrowserURL guards for the browser opener: a target that looks like a
// flag must not reach the exec'd ssh as one. The flag parser turns most of these
// away by itself; a bare "-" is the one it passes through as a positional.
func TestSSHCmdRejectsFlagLikeTarget(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())
	for _, target := range []string{"-oProxyCommand=touch /tmp/pwned", "-", "-F/dev/null"} {
		if err := sshCmd([]string{target}); err == nil {
			t.Errorf("expected a refusal for target %q", target)
		}
	}
}

func TestHostKeyLineBracketsNonDefaultPort(t *testing.T) {
	// The bracket form for a non-default port is the classic silent bug, and
	// four of the ten providers publish sshd on a dynamic non-22 port.
	cases := []struct{ host, port, want string }{
		{"203.0.113.9", "22", "203.0.113.9 ssh-ed25519 AAAA"},
		{"203.0.113.9", "", "203.0.113.9 ssh-ed25519 AAAA"},
		{"203.0.113.9", "41022", "[203.0.113.9]:41022 ssh-ed25519 AAAA"},
	}
	for _, c := range cases {
		if got := hostKeyLine(c.host, c.port, "ssh-ed25519 AAAA"); got != c.want {
			t.Errorf("hostKeyLine(%q, %q) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}

func TestSeedHostKeyIsNoOpWithoutAKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := seedHostKey("203.0.113.9", "22", ""); err != nil {
		t.Fatalf("seedHostKey with no key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", knownHostsName)); !os.IsNotExist(err) {
		t.Error("no host key is available from the platform yet, so nothing should be written")
	}
}

func TestRemoveHostPrunesOnlyTheMatchingEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := seedHostKey("203.0.113.9", "41022", "ssh-ed25519 AAAA"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seedHostKey("198.51.100.4", "22", "ssh-ed25519 BBBB"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := removeHost("203.0.113.9", "41022"); err != nil {
		t.Fatalf("removeHost: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".ssh", knownHostsName))
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if strings.Contains(string(data), "203.0.113.9") {
		t.Errorf("torn-down host should be pruned so a recycled IP is not a mismatch; got:\n%s", data)
	}
	if !strings.Contains(string(data), "198.51.100.4") {
		t.Errorf("other hosts must survive; got:\n%s", data)
	}
}
