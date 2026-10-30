package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/Aquanodeio/aq/internal/api"
)

// queue dispatches `aq queue <list|cancel>`: the lifecycle of the requests
// `aq up --when-available` creates. A row a user can create from the CLI that
// spends money unattended must be inspectable and cancellable from the CLI, so
// these two are not optional.
func queue(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: aq queue <list|cancel> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return queueList(rest)
	case "cancel":
		return queueCancel(rest)
	default:
		return fmt.Errorf("aq queue: unknown subcommand %q, expected one of list, cancel", sub)
	}
}

func queueList(args []string) error {
	fs := flag.NewFlagSet("queue list", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "Print the rows as a JSON array")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return errors.New("usage: aq queue list [--json]")
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return runQueueList(newControlClient(cred), *jsonOut, os.Stdout)
}

func runQueueList(client *api.Client, jsonOut bool, out io.Writer) error {
	rows, err := client.ListQueuedDeployments()
	if err != nil {
		return fmt.Errorf("could not list queued requests: %w", err)
	}
	if jsonOut {
		raws := make([]json.RawMessage, 0, len(rows))
		for _, r := range rows {
			raws = append(raws, r.Raw)
		}
		return printJSONValue(out, raws)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "Nothing queued. Queue a box with: aq up --when-available --gpu <model> --max-price <n>")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tGPU\tGPUS\tPROVIDER\tCEILING $/HR\tEXPIRES")
	for _, r := range rows {
		fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.GPUModel, r.GPUCount, scopeOrAny(r.ProviderScope), formatPrice(r.PriceCeilingPerHour), formatWhen(r.ExpiresAt))
	}
	return tw.Flush()
}

func queueCancel(args []string) error {
	fs := flag.NewFlagSet("queue cancel", flag.ContinueOnError)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	id, err := parseSingleID(positional, "aq queue cancel <id>")
	if err != nil {
		return err
	}
	cred, err := requireLogin()
	if err != nil {
		return err
	}
	return runQueueCancel(newControlClient(cred), id, os.Stdout)
}

func runQueueCancel(client *api.Client, id int, out io.Writer) error {
	if err := client.CancelQueuedDeployment(id); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("queued request %d not found, not yours, or no longer active", id)
		}
		return fmt.Errorf("could not cancel queued request %d: %w", id, err)
	}
	fmt.Fprintf(out, "Cancelled queued request %d. Nothing will be rented for it.\n", id)
	return nil
}
