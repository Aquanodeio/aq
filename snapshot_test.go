package main

import "testing"

// TestSnapshotFlagsParseInAnyOrder exercises the same any-order flag/positional
// contract as `aq status`/`aq down` (#204) — flags may come before or after the
// target.
func TestSnapshotFlagsParseInAnyOrder(t *testing.T) {
	for _, args := range [][]string{
		{"2884", "--name", "before-upgrade"},
		{"--name", "before-upgrade", "2884"},
	} {
		opts, err := parseSnapshotArgs(args)
		if err != nil {
			t.Fatalf("args %v: %v", args, err)
		}
		if opts.target != "2884" || opts.name != "before-upgrade" {
			t.Errorf("args %v -> %+v", args, opts)
		}
	}
}

func TestSnapshotDefaultsWorkspaceDir(t *testing.T) {
	opts, err := parseSnapshotArgs([]string{"2884"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.workspaceDir != "/workspace" {
		t.Errorf("workspaceDir = %q, want /workspace", opts.workspaceDir)
	}
}
