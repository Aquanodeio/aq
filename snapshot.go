package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// snapshotOptions configures runSnapshot. snapshot() fills in the real
// environment; tests call parseSnapshotArgs / runSnapshot directly.
type snapshotOptions struct {
	cred         *config.Credential
	target       string // deployment id, name, or project UUID
	name         string // optional snapshot label; becomes the backup_id
	workspaceDir string
}

// parseSnapshotArgs parses `aq snapshot`'s flags and positional target, letting
// them appear in any order (#204).
func parseSnapshotArgs(args []string) (snapshotOptions, error) {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	name := fs.String("name", "", "label for this snapshot (default: aq-<deploymentId>)")
	workspace := fs.String("workspace", "/workspace", "directory to capture")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return snapshotOptions{}, err
	}
	opts := snapshotOptions{name: *name, workspaceDir: *workspace}
	if len(rest) > 0 {
		opts.target = rest[0]
	}
	return opts, nil
}

// snapshot parses flags and wires the real environment into runSnapshot.
//
// `aq snapshot <deploymentId>` saves a deployment's current state on demand —
// the same one-shot capture the console's Snapshotter tab drives, reachable
// without leaving the terminal.
func snapshot(args []string) error {
	opts, err := parseSnapshotArgs(args)
	if err != nil {
		return err
	}
	if opts.target == "" {
		return fmt.Errorf("a deployment id is required — usage: aq snapshot <deploymentId>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}
	opts.cred = cred

	// Resolve the target (which may be a --name or a project UUID) to its
	// numeric deployment id up front, so the printed restore command is always
	// the id `aq deploy --snapshot` actually accepts, not a name or UUID it does
	// not understand.
	client := newControlClient(cred)
	id, err := resolveDeploymentID(client, opts.target, "snapshot")
	if err != nil {
		return err
	}
	opts.target = strconv.Itoa(id)

	res, err := runSnapshot(opts)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "✓ Saved your setup — snapshot %s\n", res.SnapshotID)
	fmt.Fprintf(os.Stdout, "\nStart it again anywhere with:\n  aq deploy --snapshot %d\n", id)
	return nil
}

// runSnapshot resolves the target and takes a one-shot snapshot of it. Reused
// by `aq down --snapshot` as the checkpoint step before terminating.
func runSnapshot(opts snapshotOptions) (api.CreateSnapshotResult, error) {
	client := newControlClient(opts.cred)

	id, err := resolveDeploymentID(client, opts.target, "snapshot")
	if err != nil {
		return api.CreateSnapshotResult{}, err
	}

	backupID := opts.name
	if backupID == "" {
		backupID = fmt.Sprintf("aq-%d", id)
	}

	return client.CreateSnapshot(id, api.CreateSnapshotRequest{
		BackupID:         backupID,
		WorkspaceDir:     opts.workspaceDir,
		IncludeProcess:   false,
		IncludeWorkspace: true,
	})
}
