package main

import (
	"os"
	"testing"
)

// TestMain points HOME and AQ_CONFIG_DIR at a throwaway directory for the whole
// package. aq now writes ~/.ssh/config, ~/.ssh/aquanode.config, and a managed
// keypair, so a test that forgot to isolate HOME would edit the developer's real
// ssh setup. Individual tests still override HOME where they need their own.
func TestMain(m *testing.M) {
	sandbox, err := os.MkdirTemp("", "aq-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", sandbox)
	os.Setenv("AQ_CONFIG_DIR", sandbox+"/config")
	code := m.Run()
	os.RemoveAll(sandbox)
	os.Exit(code)
}

// TestVersionDefault is a smoke test so `go test ./...` has something to run and
// CI is green from the first commit.
func TestVersionDefault(t *testing.T) {
	if version == "" {
		t.Fatal("version must not be empty")
	}
}

func TestResolveVersionPrefersLdflagsInjectedVersion(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "1.2.3"
	if got := resolveVersion(); got != "1.2.3" {
		t.Fatalf("resolveVersion() = %q, want the ldflags-injected %q", got, "1.2.3")
	}
}

func TestResolveVersionFallsBackToSentinelWithoutModuleVersion(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	// A test binary's build info carries no release tag (it is "" or "(devel)"),
	// which is the same state a plain `go build` from a checkout produces. The
	// dev sentinel must survive rather than leak "(devel)" to users.
	version = "0.0.0-dev"
	if got := resolveVersion(); got != "0.0.0-dev" {
		t.Fatalf("resolveVersion() = %q, want the dev sentinel to survive", got)
	}
}
