package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// volume dispatches `aq volume <sub>`, the Volume half of the
// pod/environment/volume model: /workspace, your code and data. It has
// automatic history (one point per Stop) and no manual save button:
// `ls`/`dup`/`restore`/`rm` are the whole vocabulary. `aq import` is the
// separate top-level command that CREATES a volume from a box rented
// elsewhere; it is not one of these subcommands.
func volume(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq volume <ls|dup|restore|rm> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		return volumeLs(rest)
	case "dup":
		return volumeDup(rest)
	case "restore":
		return volumeRestore(rest)
	case "rm":
		return volumeRm(rest)
	default:
		return fmt.Errorf("aq volume: unknown subcommand %q, expected one of ls, dup, restore, rm", sub)
	}
}

// volumeLsOptions configures runVolumeLs. volumeLs() fills in the real
// environment; tests call runVolumeLs directly.
type volumeLsOptions struct {
	cred   *config.Credential
	target string // optional: a volume name/id to show detail + history for
	out    io.Writer
}

// volumeLs parses `aq volume ls [<name|id>]` and wires the real environment
// into runVolumeLs.
func volumeLs(args []string) error {
	fs := flag.NewFlagSet("volume ls", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	opts := volumeLsOptions{cred: cred, out: os.Stdout}
	if len(positional) > 0 {
		opts.target = positional[0]
	}
	return runVolumeLs(opts)
}

// runVolumeLs lists every volume the caller owns (no target), or one
// volume's detail and full point history (target given).
func runVolumeLs(opts volumeLsOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}
	client := newControlClient(opts.cred)

	if opts.target == "" {
		volumes, err := client.ListVolumes()
		if err != nil {
			return fmt.Errorf("could not list volumes: %w", err)
		}
		printVolumes(out, volumes)
		return nil
	}

	volumeID, err := resolveVolumeID(client, opts.target)
	if err != nil {
		return err
	}
	v, err := client.GetVolume(volumeID)
	if err != nil {
		return fmt.Errorf("could not fetch volume %q: %w", opts.target, err)
	}
	printVolumeDetail(out, *v)
	return nil
}

// printVolumes renders the volume list as a simple aligned table.
func printVolumes(out io.Writer, volumes []api.Volume) {
	if len(volumes) == 0 {
		fmt.Fprintln(out, "No volumes yet.")
		return
	}
	fmt.Fprintf(out, "%-24s  %-10s  %-24s  %s\n", "NAME", "SIZE", "ATTACHED", "LAST SAVED")
	for _, v := range volumes {
		fmt.Fprintf(out, "%-24s  %-10s  %-24s  %s\n", truncate(v.Name, 24), formatPodSizePtr(v.SizeBytes), volumeAttachedLabel(v), orDashPtr(v.HeadSavedAt))
	}
}

// volumeAttachedLabel renders a volume's attachment, by the pod's NAME
// (falling back to its id if the name is somehow absent) rather than a bare
// yes/no: "-" unattached, the pod's name while its pod is Running, and
// "<name> (stopped)" when the pod still owns this volume but isn't running
// right now, three states, never collapsed into a boolean.
func volumeAttachedLabel(v api.Volume) string {
	if v.AttachedPodID == nil {
		return "-"
	}
	name := *v.AttachedPodID
	if v.AttachedPodName != nil && *v.AttachedPodName != "" {
		name = *v.AttachedPodName
	}
	if v.Running {
		return name
	}
	return name + " (stopped)"
}

// printVolumeDetail renders one volume's fields plus its point history,
// provenance is the ONLY thing that created a point (a Stop, or an idle
// auto-stop); there is no manual save point.
func printVolumeDetail(out io.Writer, v api.Volume) {
	fmt.Fprintf(out, "%s (%s)\n", v.Name, v.ID)
	fmt.Fprintf(out, "  Size: %s\n", formatPodSizePtr(v.SizeBytes))
	fmt.Fprintf(out, "  Mount path: %s\n", orDash(v.MountPath))
	fmt.Fprintf(out, "  Attached to pod: %s\n", volumeAttachedLabel(v))
	state := v.SaveState
	if state == "" {
		state = "unknown"
	}
	fmt.Fprintf(out, "  Last saved: %s (%s)\n", orDashPtr(v.HeadSavedAt), state)
	if v.LastSaveError != nil && *v.LastSaveError != "" {
		fmt.Fprintf(out, "  Last save failed: %s\n", *v.LastSaveError)
	}

	fmt.Fprintln(out, "\nHistory:")
	if len(v.Points) == 0 {
		fmt.Fprintln(out, "  (none yet)")
		return
	}
	fmt.Fprintf(out, "  %-36s  %-24s  %-10s  %s\n", "ID", "CREATED", "FROM", "LABEL")
	for _, p := range v.Points {
		label := "-"
		if p.Label != nil && *p.Label != "" {
			label = *p.Label
		}
		fmt.Fprintf(out, "  %-36s  %-24s  %-10s  %s\n", p.ID, orDash(p.CreatedAt), orDash(p.Provenance), label)
	}
}

// volumeDupOptions configures runVolumeDup. volumeDup() fills in the real
// environment; tests call runVolumeDup directly.
type volumeDupOptions struct {
	cred   *config.Credential
	target string // volume name or id
	name   string // name for the duplicate
	out    io.Writer
}

// volumeDup parses `aq volume dup <name|id> <new-name>` and wires the real
// environment into runVolumeDup.
func volumeDup(args []string) error {
	fs := flag.NewFlagSet("volume dup", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq volume dup <name|id> <new-name>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runVolumeDup(volumeDupOptions{cred: cred, target: positional[0], name: positional[1], out: os.Stdout})
}

// runVolumeDup forks a volume into a brand new one at its latest point: an
// honest fork, writes never merge back.
func runVolumeDup(opts volumeDupOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	volumeID, err := resolveVolumeID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.DuplicateVolume(volumeID, opts.name)
	if err != nil {
		return fmt.Errorf("could not duplicate volume %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Duplicated into %q (id %s). Attach it to a pod from the console or the New pod flow.\n", res.Name, res.ID)
	return nil
}

// volumeRestoreOptions configures runVolumeRestore. volumeRestore() fills in
// the real environment; tests call runVolumeRestore directly.
type volumeRestoreOptions struct {
	cred    *config.Credential
	target  string // volume name or id
	pointID string
	out     io.Writer
}

// volumeRestore parses `aq volume restore <name|id> <pointId>` and wires the
// real environment into runVolumeRestore.
func volumeRestore(args []string) error {
	fs := flag.NewFlagSet("volume restore", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 || positional[0] == "" || positional[1] == "" {
		return errors.New("usage: aq volume restore <name|id> <pointId>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runVolumeRestore(volumeRestoreOptions{cred: cred, target: positional[0], pointID: positional[1], out: os.Stdout})
}

// runVolumeRestore sets a volume's head to an earlier point. Refused (409)
// while the volume is attached to a running pod: this is never "newest by
// time", only ever the exact point named.
func runVolumeRestore(opts volumeRestoreOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	volumeID, err := resolveVolumeID(client, opts.target)
	if err != nil {
		return err
	}

	res, err := client.RestoreVolumePoint(volumeID, opts.pointID)
	if err != nil {
		return fmt.Errorf("could not restore volume %q to point %s: %w", opts.target, opts.pointID, err)
	}

	fmt.Fprintf(out, "✓ %s is now at point %s.\n", res.Name, opts.pointID)
	return nil
}

// volumeRmOptions configures runVolumeRm. volumeRm() fills in the real
// environment; tests call runVolumeRm directly.
type volumeRmOptions struct {
	cred   *config.Credential
	target string // volume name or id
	out    io.Writer
}

// volumeRm parses `aq volume rm <name|id>` and wires the real environment
// into runVolumeRm.
func volumeRm(args []string) error {
	fs := flag.NewFlagSet("volume rm", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return errors.New("usage: aq volume rm <name|id>")
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	return runVolumeRm(volumeRmOptions{cred: cred, target: positional[0], out: os.Stdout})
}

// runVolumeRm deletes a volume and its whole history. Refused (409) while
// attached.
func runVolumeRm(opts volumeRmOptions) error {
	out := opts.out
	if out == nil {
		out = os.Stdout
	}

	client := newControlClient(opts.cred)
	volumeID, err := resolveVolumeID(client, opts.target)
	if err != nil {
		return err
	}

	if err := client.DeleteVolume(volumeID); err != nil {
		return fmt.Errorf("could not delete volume %q: %w", opts.target, err)
	}

	fmt.Fprintf(out, "✓ Deleted volume %q.\n", opts.target)
	return nil
}
