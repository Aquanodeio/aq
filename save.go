package main

import (
	"flag"
	"fmt"
	"os"
)

// snapshot parses `aq save host:<alias>` and dispatches to ogre's own
// snapshot verb on a detached box.
//
// This command used to also save a MANAGED pod's current state into a named
// lineage (POST /setups/:id/snapshot). That mechanism is gone under the
// pod/environment/volume model: a pod's environment and volume are saved
// automatically, silently, on every Stop (see stop.go) — there is no
// standalone "save" button or verb for a managed pod any more (spec: "no
// save button anywhere in the everyday flow"). `aq save` therefore survives
// ONLY for a detached (BYO-bucket, no Aquanode account) box, where ogre's own
// `snapshot` verb captures into the box's configured remote and there is no
// Stop to save on.
func snapshot(args []string) error {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("usage: aq save host:<alias>")
	}

	alias, ok := parseHostTarget(positional[0])
	if !ok {
		return fmt.Errorf("aq save only takes a detached host target now (host:<alias>); a managed pod's environment and volume save automatically on `aq stop`")
	}
	return runDetached(detachedOptions{verb: "save", alias: alias, out: os.Stdout, errOut: os.Stderr})
}
