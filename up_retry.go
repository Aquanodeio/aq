package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
)

// `aq up --retry-until-up` keeps asking while the server has nothing to rent.
//
// The server's refusal for "no offer matches right now" is a 409 whose envelope
// carries no data code: any code means a different, permanent answer (a coded
// 409 is something the caller must fix), and a 402 is a funding answer. So the
// loop keys on the SHAPE of the refusal, never on a provider name: it holds for
// every provider. A 503 is the server saying it could not look, which says
// nothing about supply and created nothing: warn and keep going. Everything
// else (a network error, a 504, a 5xx) is UNKNOWN whether the POST created a
// row, so it stops rather than risk renting twice.

const (
	defaultRetryGap         = 30 * time.Second
	defaultRetryReportEvery = 10 * time.Minute

	// noOfferMatches is the phrase a bounded run ends on. The inference
	// harness's rent.sh greps for exactly these words to tell "supply gone,
	// nothing created" from a failure, so it must stay verbatim.
	noOfferMatches = "no offer matches"
)

// validateRetryFlags refuses the combinations the retry loop cannot honour.
func validateRetryFlags(fs *flag.FlagSet, retryUntilUp bool, retryFor time.Duration, whenAvailable, hasTarget bool) error {
	forSet := isFlagSet(fs, "retry-for")
	if !retryUntilUp && !forSet {
		return nil
	}
	if forSet && !retryUntilUp {
		return errors.New("--retry-for only applies with --retry-until-up")
	}
	if forSet && retryFor <= 0 {
		return errors.New("--retry-for needs a positive duration, e.g. 25m")
	}
	if whenAvailable {
		return errors.New("--retry-until-up cannot be combined with --when-available: one waits in this terminal, the other queues on the server")
	}
	if hasTarget {
		return errors.New("--retry-until-up rents a new box; it cannot target a host")
	}
	return nil
}

// isCodelessConflict reports whether err is the server's "nothing to rent"
// refusal: HTTP 409 with no `code` in the envelope's data.
func isCodelessConflict(err error) bool {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		return false
	}
	if len(apiErr.Data) == 0 {
		return true
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(apiErr.Data, &body) != nil {
		// Data that is not an object cannot be read as a code: unknown, so stop.
		return false
	}
	return body.Code == ""
}

// refusalReason is the `data.refusal` the server names for a try-only refusal
// (no_capacity, quota, disabled), or "" when the body carries none. An old
// server sends no data, so absent stays absent and is never guessed.
func refusalReason(err error) string {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || len(apiErr.Data) == 0 {
		return ""
	}
	var body struct {
		Refusal string `json:"refusal"`
	}
	if json.Unmarshal(apiErr.Data, &body) != nil {
		return ""
	}
	return body.Refusal
}

// reasonSuffix renders ", reason=<refusal>" for the wait line and the bounded
// end; empty when the server named none.
func reasonSuffix(refusal string) string {
	if refusal == "" {
		return ""
	}
	return ", reason=" + refusal
}

// endSentence makes sure a server message ends its sentence exactly once, so
// the text appended after it never follows a missing or a doubled period.
func endSentence(msg string) string {
	if msg == "" || strings.HasSuffix(msg, ".") || strings.HasSuffix(msg, "!") || strings.HasSuffix(msg, "?") {
		return msg
	}
	return msg + "."
}

func isCouldNotLook(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusServiceUnavailable
}

type upAnswer struct {
	res *api.UpResult
	err error
}

// upWithRetry is client.Up when retrying is off, and the retry loop when on.
func upWithRetry(client *api.Client, opts upOptions, req api.UpRequest) (*api.UpResult, error) {
	if !opts.retryUntilUp {
		return client.Up(req)
	}
	gap, every := opts.retryGap, opts.retryReportEvery
	if gap <= 0 {
		gap = defaultRetryGap
	}
	if every <= 0 {
		every = defaultRetryReportEvery
	}
	intr := opts.interrupt
	if intr == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		// Stopped on return so a Ctrl-C during the later provisioning wait keeps
		// its default meaning.
		defer signal.Stop(ch)
		intr = ch
	}

	start := opts.now()
	lastReport := time.Time{}
	attempts := 0
	for {
		attempts++
		answer := make(chan upAnswer, 1)
		go func() {
			res, err := client.Up(req)
			answer <- upAnswer{res, err}
		}()
		var a upAnswer
		select {
		case a = <-answer:
		case <-intr:
			// The POST is in flight and cannot be recalled: the server may
			// already have created a deployment.
			fmt.Fprintf(opts.errOut, "Interrupted while a request to rent was in flight (attempt %d). A box may have been created: check `aq ls`.\n", attempts)
			return nil, &exitError{code: 130, err: errors.New("interrupted during a rent request")}
		}
		if a.err == nil {
			return a.res, nil
		}
		refused, unavailable := isCodelessConflict(a.err), isCouldNotLook(a.err)
		if !refused && !unavailable {
			return nil, a.err
		}

		elapsed := opts.now().Sub(start)
		if opts.retryFor > 0 && elapsed+gap > opts.retryFor {
			if refused {
				return nil, &exitError{code: exitFailure, err: fmt.Errorf("%s after %d attempts over %s%s (last answer: %s); nothing was rented", noOfferMatches, attempts, roundElapsed(elapsed), reasonSuffix(refusalReason(a.err)), a.err.Error())}
			}
			return nil, fmt.Errorf("gave up after %d attempts over %s and the last answer was that the server could not look (%s); nothing is known to be rented", attempts, roundElapsed(elapsed), a.err.Error())
		}
		if lastReport.IsZero() || opts.now().Sub(lastReport) >= every {
			lastReport = opts.now()
			if refused {
				bound := "until a box is up"
				if opts.retryFor > 0 {
					bound = "for up to " + opts.retryFor.String()
				}
				fmt.Fprintf(opts.errOut, "Waiting for supply (attempt %d, %s elapsed%s): %s Trying every %s %s; Ctrl-C to stop.\n", attempts, roundElapsed(elapsed), reasonSuffix(refusalReason(a.err)), endSentence(a.err.Error()), gap, bound)
			} else {
				fmt.Fprintf(opts.errOut, "Warning: the server could not look at supply (attempt %d, %s elapsed): %s. Trying again every %s.\n", attempts, roundElapsed(elapsed), a.err.Error(), gap)
			}
		}

		timer := time.NewTimer(gap)
		select {
		case <-timer.C:
		case <-intr:
			timer.Stop()
			fmt.Fprintf(opts.errOut, "Stopped after %d attempts over %s. Nothing was rented.\n", attempts, roundElapsed(opts.now().Sub(start)))
			return nil, &exitError{code: 130, err: errors.New("interrupted while waiting for supply")}
		}
	}
}

func roundElapsed(d time.Duration) time.Duration {
	if d >= time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(100 * time.Millisecond)
}
