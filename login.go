package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

// loginOptions configures runLogin. Defaults are filled in by login() so tests
// can inject an httptest base URL, a fast poll interval, and a buffer writer.
type loginOptions struct {
	apiURL       string
	clientName   string
	out          io.Writer
	openBrowser  bool
	pollInterval time.Duration // 0 → use the server-advertised interval
	now          func() time.Time
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
		interval = 5 * time.Second
	}
	if opts.pollInterval > 0 {
		interval = opts.pollInterval
	}

	expiresIn := start.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	deadline := opts.now().Add(time.Duration(expiresIn) * time.Second)

	for {
		if opts.now().After(deadline) {
			return errors.New("login timed out before approval — run `aq login` again")
		}
		time.Sleep(interval)

		poll, err := client.PollDevice(start.DeviceCode)
		if err != nil {
			// A transient poll error shouldn't kill the whole login; keep waiting
			// unless the server explicitly rejected the device code.
			var apiErr *api.APIError
			if errors.As(err, &apiErr) {
				return fmt.Errorf("login failed: %s", apiErr.Message)
			}
			continue
		}

		switch poll.Status {
		case "pending":
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
