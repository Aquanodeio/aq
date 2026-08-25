package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
)

// lsCmd parses `aq ls` and prints the team's deployments.
//
// `aq setups` answers "what have I built"; this answers "what is running and
// costing me money right now" — the question you ask before walking away from
// the terminal. Live boxes only by default, since a closed one cannot surprise
// you on a bill.
func lsCmd(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	all := fs.Bool("all", false, "Include closed and failed deployments, not just live ones")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}

	cred, err := requireLogin()
	if err != nil {
		return err
	}

	client := newControlClient(cred)
	deps, err := client.ListDeployments()
	if err != nil {
		return fmt.Errorf("could not list deployments: %w", err)
	}

	printDeployments(os.Stdout, filterDeployments(deps, *all), *all, time.Now())
	return nil
}

// filterDeployments drops rows without a usable id, and terminal ones unless
// --all was passed.
func filterDeployments(deps []api.Deployment, all bool) []api.Deployment {
	var out []api.Deployment
	for _, d := range deps {
		if d.ID <= 0 {
			continue
		}
		if !all && isClosedStatus(d.Status) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// printDeployments renders the table.
func printDeployments(out io.Writer, deps []api.Deployment, all bool, now time.Time) {
	if len(deps) == 0 {
		if all {
			fmt.Fprintln(out, "No deployments yet — run `aq up` to start one.")
		} else {
			fmt.Fprintln(out, "Nothing running — run `aq up` to start a box, or `aq ls --all` to see closed ones.")
		}
		return
	}

	fmt.Fprintf(out, "%-6s  %-24s  %-12s  %-16s  %-14s  %-16s  %s\n",
		"ID", "NAME", "STATUS", "GPU", "PROVIDER", "RATE/HR", "AGE")
	for _, d := range deps {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(out, "%-6d  %-24s  %-12s  %-16s  %-14s  %-16s  %s\n",
			d.ID, truncate(name, 24), orDash(d.Status), formatGPU(d.GPU, d.GPUCount),
			orDash(d.Provider), formatRate(float64(d.PricePerSecond), d.Currency),
			formatAge(d.CreatedAt, now))
	}
}

// formatGPU renders the GPU column, showing the multiplier only when there is
// more than one — the whole point of surfacing count at all is that an 8-GPU
// box should not look like a 1-GPU box in a list you scan for spend.
func formatGPU(model string, count int) string {
	model = strings.TrimSpace(model)
	if model == "" {
		if count > 1 {
			return fmt.Sprintf("%dx ?", count)
		}
		return "-"
	}
	if count > 1 {
		return fmt.Sprintf("%dx %s", count, model)
	}
	return model
}

// formatRate renders an hourly rate WITH its currency code, never a bare "$".
//
// deployments.price_per_second is denominated in the provider's own currency
// (deployments.currency: USD, AKT, USPON, ...) and is converted to USD only
// when a billing bucket is minted. Printing "$" in front of an Akash row's
// rate would state AKT as dollars. A row with no currency recorded renders the
// number against "?" rather than assuming the common case — an unlabelled rate
// is worse than an obviously-unknown one.
func formatRate(pricePerSecond float64, currency string) string {
	if pricePerSecond <= 0 {
		return "-"
	}
	code := strings.TrimSpace(strings.ToUpper(currency))
	if code == "" {
		code = "?"
	}
	return fmt.Sprintf("%.4f %s", pricePerSecond*3600, code)
}

// formatAge renders how long a deployment has existed, coarsely — the column
// exists to catch a box you forgot about, and minutes-level precision on a
// three-day-old box helps nobody.
func formatAge(createdAt string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAt))
	if err != nil {
		return "-"
	}
	d := now.Sub(t)
	if d < 0 {
		return "-"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// truncate keeps a cell inside its column. Orchestrator-generated names like
// "Tough A30 from Us-central-1" run past 24 characters and would otherwise
// shove every following column out of alignment on that row alone — which
// makes the whole table harder to scan than the lost characters were worth.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
