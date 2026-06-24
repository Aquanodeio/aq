package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

const (
	// defaultPollInterval is the cadence used when the server advertises none.
	defaultPollInterval = 5 * time.Second
	// slowDownIncrement is how much each `slow_down` response widens the poll
	// interval (RFC 8628 §3.5: back off by at least 5 seconds).
	slowDownIncrement = 5 * time.Second
)

// loginOptions configures runLogin. Defaults are filled in by login() so tests
// can inject an httptest base URL, a fast poll interval, and a buffer writer.
type loginOptions struct {
	apiURL       string
	clientName   string
	out          io.Writer
	openBrowser  bool
	pollInterval time.Duration       // 0 → use (and honor) the server-advertised interval
	now          func() time.Time    // nil → time.Now
	sleep        func(time.Duration) // nil → time.Sleep (injectable so tests don't really wait)
}

// login wires the real environment (stdout, hostname, browser) into runLogin.
func login(args []string) error {
	host, _ := os.Hostname()
	return runLogin(loginOptions{
		apiURL:      config.APIURL(),
		clientName:  host,
		out:         os.Stdout,
		openBrowser: os.Getenv("AQ_NO_BROWSER") == "",
		now:         time.Now,
	})
}

// runLogin drives the device-grant pairing: start → show code → poll until the
// user approves in the console → persist the issued CLI credential.
func runLogin(opts loginOptions) error {
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.sleep == nil {
		opts.sleep = time.Sleep
	}

	client := api.New(opts.apiURL)
	start, err := client.StartDevice(opts.clientName, nil)
	if err != nil {
		return fmt.Errorf("could not start login: %w", err)
	}

	fmt.Fprintf(opts.out, "\nTo connect this CLI to your Aquanode account, visit:\n\n    %s\n\n", start.VerificationURIComplete)
	fmt.Fprintf(opts.out, "and confirm this pairing code:\n\n    %s\n\n", start.UserCode)
	if len(start.Scopes) > 0 {
		fmt.Fprintf(opts.out, "Requested access: %v\n\n", start.Scopes)
	}

	if opts.openBrowser {
		if err := openBrowser(start.VerificationURIComplete); err == nil {
			fmt.Fprintln(opts.out, "Opened your browser to approve...")
		}
	}
	fmt.Fprintln(opts.out, "Waiting for approval...")

	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	// A pinned pollInterval (tests) overrides everything and is never widened by
	// the server; otherwise the cadence is server-driven and may only grow.
	serverDriven := opts.pollInterval <= 0
	if !serverDriven {
		interval = opts.pollInterval
	}

	expiresIn := start.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	deadline := opts.now().Add(time.Duration(expiresIn) * time.Second)

	first := true
	for {
		if opts.now().After(deadline) {
			return errors.New("login timed out before approval — run `aq login` again")
		}
		// Poll immediately on the first iteration; only sleep *between* polls so we
		// don't add a full interval of latency before the first status check (#207).
		if !first {
			opts.sleep(interval)
		}
		first = false

		poll, err := client.PollDevice(start.DeviceCode)
		if err != nil {
			// A transient poll error shouldn't kill the whole login; keep waiting
			// unless the server explicitly rejected the device code.
			var apiErr *api.APIError
			if errors.As(err, &apiErr) {
				// Some servers signal "poll slower" (RFC 8628 §3.5) as a 4xx
				// error rather than a status — that's backoff, not a failure.
				if isSlowDown(apiErr.Message) {
					interval = backoffInterval(interval, 0, serverDriven)
					continue
				}
				return fmt.Errorf("login failed: %s", apiErr.Message)
			}
			continue
		}

		switch poll.Status {
		case "pending":
			// Honor a larger server-advertised cadence on every poll.
			interval = adoptInterval(interval, poll.Interval, serverDriven)
			continue
		case "slow_down":
			interval = backoffInterval(interval, poll.Interval, serverDriven)
			continue
		case "approved":
			cred := &config.Credential{
				APIURL:  opts.apiURL,
				Token:   poll.Token,
				TeamID:  poll.TeamID,
				KeyName: poll.KeyName,
				Scopes:  poll.Scopes,
			}
			if err := config.Save(cred); err != nil {
				return fmt.Errorf("login approved but saving the credential failed: %w", err)
			}
			label := poll.KeyName
			if label == "" {
				label = "this CLI"
			}
			fmt.Fprintf(opts.out, "\n✓ Connected as %s.\n", label)
			fmt.Fprintln(opts.out, "You can close the browser tab. Run `aq whoami` to confirm.")
			return nil
		case "denied":
			return errors.New("pairing was denied in the console")
		case "expired":
			return errors.New("pairing expired — run `aq login` again")
		case "consumed":
			return errors.New("this pairing was already used — run `aq login` again")
		default:
			return fmt.Errorf("unexpected pairing status %q", poll.Status)
		}
	}
}

// adoptInterval widens the poll cadence to a larger server-advertised interval
// (seconds). It only ever slows down, and never overrides a pinned test cadence.
func adoptInterval(current time.Duration, advertised int, serverDriven bool) time.Duration {
	if !serverDriven || advertised <= 0 {
		return current
	}
	if d := time.Duration(advertised) * time.Second; d > current {
		return d
	}
	return current
}

// backoffInterval applies a `slow_down` backoff (RFC 8628 §3.5): the cadence
// grows by at least slowDownIncrement, and adopts a larger advertised interval
// if the server sent one. A pinned test cadence is left untouched.
func backoffInterval(current time.Duration, advertised int, serverDriven bool) time.Duration {
	if !serverDriven {
		return current
	}
	next := current + slowDownIncrement
	return adoptInterval(next, advertised, serverDriven)
}

// isSlowDown reports whether a server message is the RFC 8628 "poll slower"
// signal rather than a real failure.
func isSlowDown(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "slow_down") || strings.Contains(m, "slow down")
}

// logout removes the stored credential.
func logout(args []string) error {
	existed, err := config.Clear()
	if err != nil {
		return err
	}
	if existed {
		fmt.Println("Logged out — stored credential removed.")
	} else {
		fmt.Println("Not logged in — nothing to remove.")
	}
	return nil
}

// whoami prints the current login state.
func whoami(args []string) error {
	cred, err := config.Load()
	if err != nil {
		return err
	}
	if cred == nil || cred.Token == "" {
		fmt.Println("Not logged in. Run `aq login` to connect this CLI.")
		return nil
	}
	label := cred.KeyName
	if label == "" {
		label = "this CLI"
	}
	fmt.Printf("Logged in as %s", label)
	if cred.TeamID != "" {
		fmt.Printf(" (team %s)", cred.TeamID)
	}
	fmt.Println()
	return nil
}
