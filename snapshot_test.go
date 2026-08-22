package main

import "testing"

// TestSnapshotFlagsParseInAnyOrder exercises the same any-order flag/positional
// contract as `aq status`/`aq down` — flags may come before or after the target.
func TestSnapshotFlagsParseInAnyOrder(t *testing.T) {
	for _, args := range [][]string{
		{"comfyui", "--name", "before-upgrade"},
		{"--name", "before-upgrade", "comfyui"},
	} {
		opts, err := parseSnapshotArgs(args)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if opts.target != "comfyui" || opts.name != "before-upgrade" {
			t.Errorf("args %v -> %+v", args, opts)
		}
	}
}

func TestSnapshotDefaultsPathDir(t *testing.T) {
	opts, err := parseSnapshotArgs([]string{"comfyui"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.pathDir != "/workspace" {
		t.Errorf("pathDir = %q, want /workspace", opts.pathDir)
	}
}

func TestSnapshotPathFlagOverridesDefault(t *testing.T) {
	opts, err := parseSnapshotArgs([]string{"comfyui", "--path", "/data"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.pathDir != "/data" {
		t.Errorf("pathDir = %q, want /data", opts.pathDir)
	}
}

// TestLineageNamedOnceThenRemembered exercises the local first-save cache
// markLineageNamed/loadNamedLineages round trip: a setup id (a uuid string,
// not a number) isn't recorded until markLineageNamed is called for it, and
// is remembered after.
func TestLineageNamedOnceThenRemembered(t *testing.T) {
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())

	const setupA = "11111111-1111-1111-1111-111111111111"
	const setupB = "22222222-2222-2222-2222-222222222222"

	if loadNamedLineages()[setupA] {
		t.Fatal("setupA should not be recorded as named yet")
	}

	markLineageNamed(setupA)

	if !loadNamedLineages()[setupA] {
		t.Fatal("setupA should be recorded as named after markLineageNamed")
	}
	if loadNamedLineages()[setupB] {
		t.Fatal("marking setupA must not affect setupB")
	}
}
