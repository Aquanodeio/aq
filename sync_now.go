package main

import (
	"flag"
	"fmt"
	"os"
)

// syncNow parses `aq sync-now host:<alias>` and dispatches to ogre's own push
// verb on a detached box.
//
// This command used to also force a MANAGED pod's sync tick outside its own
// schedule (POST /setups/:id/sync). That mechanism is gone under the
// pod/environment/volume model: a running pod's volume saves itself on a
// leader-elected periodic tick with no user-facing button (spec mechanism 9),
// so there is nothing left for a managed target to force. `aq sync-now`
// therefore survives ONLY for a detached (BYO-bucket, no Aquanode account)
// box, which runs no scheduler at all — "force the tick now" is the only
// form the verb has there, and the real one, not a stand-in.
func syncNow(args []string) error {
	fs := flag.NewFlagSet("sync-now", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 || positional[0] == "" {
		return fmt.Errorf("usage: aq sync-now host:<alias>")
	}

	alias, ok := parseHostTarget(positional[0])
	if !ok {
		return fmt.Errorf("aq sync-now only takes a detached host target now (host:<alias>); a managed pod's volume saves itself on a periodic tick")
	}
	return runDetached(detachedOptions{verb: "sync-now", alias: alias, out: os.Stdout, errOut: os.Stderr})
}
