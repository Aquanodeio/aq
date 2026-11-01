package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/config"
)

// retryServer answers POST /deployments/up from a script of responses; the
// last entry repeats. The shapes are the real envelope the orchestrator sends.
type retryServer struct {
	script []func(w http.ResponseWriter)
	calls  int32
}

func (s *retryServer) handler() http.Handler {
	mux := http.NewServeMux()
	stubDeploymentList(mux)
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, []map[string]any{{"id": "k1", "name": "x", "public_key": "ssh-ed25519 AAAA x"}})
	})
	mux.HandleFunc("/deployments/up", func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&s.calls, 1)) - 1
		if n >= len(s.script) {
			n = len(s.script) - 1
		}
		s.script[n](w)
	})
	mux.HandleFunc("/deployments/77/status", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{"deploymentId": 77, "status": "ACTIVE", "deployment": map[string]any{"id": 77, "status": "ACTIVE"}})
	})
	return mux
}

func refuse409(w http.ResponseWriter) { writeError(w, http.StatusConflict, "no matching offer") }

func codedConflict(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "pick another region", "data": map[string]any{"code": "REGION_LOCKED"}})
}

func couldNotLook(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "feed unreadable")
}

func rented(w http.ResponseWriter) {
	writeData(w, map[string]any{"deploymentId": 77, "projectId": "p", "status": "PENDING"})
}

// fakeClock advances one minute per read, so elapsed time is deterministic.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { c.t = c.t.Add(time.Minute); return c.t }

func runRetry(t *testing.T, s *retryServer, mutate func(*upOptions)) (error, string, int32) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA x")
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	var out, errOut bytes.Buffer
	opts := upOptions{
		cred:             &config.Credential{APIURL: srv.URL, Token: "aq_sk_test", TeamID: "team-1"},
		out:              &out,
		errOut:           &errOut,
		pollInterval:     time.Millisecond,
		timeout:          time.Minute,
		now:              clock.now,
		retryUntilUp:     true,
		retryGap:         time.Millisecond,
		retryReportEvery: 3 * time.Minute,
	}
	if mutate != nil {
		mutate(&opts)
	}
	err := runUp(opts)
	return err, out.String() + errOut.String(), atomic.LoadInt32(&s.calls)
}

func TestRetryUntilUpRetriesCodelessConflictThenRents(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){refuse409, refuse409, refuse409, rented}}
	err, out, calls := runRetry(t, s, nil)
	if err != nil {
		t.Fatalf("runUp: %v\n%s", err, out)
	}
	if calls != 4 {
		t.Errorf("want 4 POSTs, got %d", calls)
	}
	if !strings.Contains(out, "Waiting for supply (attempt 1") || !strings.Contains(out, "Deployment #77 created") {
		t.Errorf("missing refusal line or rent line:\n%s", out)
	}
	if strings.Contains(out, noOfferMatches) {
		t.Errorf("a successful run must not print the supply-gone phrase:\n%s", out)
	}
}

func TestRetryUntilUpReportsOnFirstRefusalThenOnCadence(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){refuse409, refuse409, refuse409, refuse409, refuse409, rented}}
	_, out, _ := runRetry(t, s, nil)
	if n := strings.Count(out, "Waiting for supply (attempt"); n < 2 || n >= 5 {
		t.Errorf("want a line on the first refusal and then only on the cadence, got %d:\n%s", n, out)
	}
}

func TestRetryUntilUpStopsAtOnceOnCodedConflict(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){codedConflict}}
	err, _, calls := runRetry(t, s, nil)
	if err == nil || !strings.Contains(err.Error(), "could not start deployment") || calls != 1 {
		t.Fatalf("want one POST and a stop, got calls=%d err=%v", calls, err)
	}
}

func TestRetryUntilUpStopsAtOnceOnPaymentRequired(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){func(w http.ResponseWriter) { writeError(w, http.StatusPaymentRequired, "top up") }}}
	err, _, calls := runRetry(t, s, nil)
	if err == nil || calls != 1 {
		t.Fatalf("want one POST and a stop, got calls=%d err=%v", calls, err)
	}
}

func TestRetryUntilUpStopsOnUnknownOutcomeServerError(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){func(w http.ResponseWriter) { writeError(w, http.StatusInternalServerError, "boom") }}}
	err, _, calls := runRetry(t, s, nil)
	if err == nil || calls != 1 {
		t.Fatalf("a 500 may have created a row, so it must not retry: calls=%d err=%v", calls, err)
	}
}

func TestRetryUntilUpWarnsOnJSON503AndKeepsGoing(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){couldNotLook, rented}}
	err, out, calls := runRetry(t, s, nil)
	if err != nil || calls != 2 || !strings.Contains(out, "could not look at supply") {
		t.Fatalf("want a warning and a second POST: calls=%d err=%v\n%s", calls, err, out)
	}
}

func TestRetryUntilUpKeepsGoingOnGatewayHTMLCouldNotLook(t *testing.T) {
	page := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<html>503</html>"))
	}
	s := &retryServer{script: []func(http.ResponseWriter){page, rented}}
	err, _, calls := runRetry(t, s, nil)
	if err != nil || calls != 2 {
		t.Fatalf("a gateway 503 page must retry: calls=%d err=%v", calls, err)
	}
}

func TestRetryForEndsWithNoOfferMatches(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){refuse409}}
	err, _, calls := runRetry(t, s, func(o *upOptions) { o.retryFor = 5 * time.Minute })
	if err == nil || !strings.Contains(err.Error(), "no offer matches") || !strings.Contains(err.Error(), "nothing was rented") {
		t.Fatalf("want the supply-gone phrase, got %v", err)
	}
	if calls < 2 {
		t.Errorf("must have retried before giving up, calls=%d", calls)
	}
}

// refusal409 is the exact body the orchestrator sends for a try-only refusal:
// a codeless 409 whose data carries only the refusal.
func refusal409(reason, message string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"error":` + strconv.Quote(message) + `,"data":{"refusal":"` + reason + `"},"timestamp":"2026-11-02T00:00:00.000Z"}`))
	}
}

func TestRetryTreatsEveryRefusalAsAWaitAndNamesIt(t *testing.T) {
	msgs := map[string]string{
		"no_capacity": `No box is free right now matching gpu ~ "H100", provider = aws. Nothing was rented. Keep trying.`,
		"quota":       `Our provider quota blocks this offer matching gpu ~ "H100", provider = aws. Nothing was rented. Waiting will not clear it.`,
		"disabled":    `Renting this provider is switched off matching gpu ~ "H100", provider = aws. Nothing was rented. Waiting will not clear it.`,
	}
	for reason, msg := range msgs {
		t.Run(reason, func(t *testing.T) {
			s := &retryServer{script: []func(http.ResponseWriter){refusal409(reason, msg), refusal409(reason, msg), rented}}
			err, out, calls := runRetry(t, s, func(o *upOptions) { o.retryFor = 25 * time.Minute })
			if err != nil || calls != 3 {
				t.Fatalf("a %s refusal must be waited on: calls=%d err=%v\n%s", reason, calls, err, out)
			}
			want := "Waiting for supply (attempt 1, "
			if !strings.Contains(out, want) || !strings.Contains(out, ", reason="+reason+"): "+msg+" Trying every 1ms for up to 25m0s; Ctrl-C to stop.\n") {
				t.Errorf("wait line off the contract:\n%s", out)
			}
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "Waiting for supply") && strings.Contains(line, "..") {
					t.Errorf("double period: %s", line)
				}
			}
		})
	}
}

func TestWaitLineWithoutRetryForSaysUntilABoxIsUp(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){refusal409("no_capacity", "No box is free right now. Nothing was rented. Keep trying."), rented}}
	_, out, _ := runRetry(t, s, nil)
	if !strings.Contains(out, " Trying every 1ms until a box is up; Ctrl-C to stop.\n") {
		t.Errorf("want the unbounded wording:\n%s", out)
	}
}

func TestWaitLineOmitsReasonWhenServerNamesNone(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){refuse409, rented}}
	_, out, _ := runRetry(t, s, nil)
	if strings.Contains(out, "reason=") {
		t.Errorf("an old server names no reason, so none is printed:\n%s", out)
	}
	if !strings.Contains(out, "elapsed): no matching offer. Trying every") {
		t.Errorf("a message without a period gets exactly one:\n%s", out)
	}
}

func TestBoundedEndNamesTheReason(t *testing.T) {
	msg := "No box is free right now. Nothing was rented. Keep trying."
	s := &retryServer{script: []func(http.ResponseWriter){refusal409("no_capacity", msg)}}
	err, _, _ := runRetry(t, s, func(o *upOptions) { o.retryFor = 5 * time.Minute })
	if err == nil {
		t.Fatal("want a bounded end")
	}
	got := err.Error()
	if !strings.HasPrefix(got, "no offer matches after ") || !strings.Contains(got, ", reason=no_capacity (last answer: "+msg+"); nothing was rented") {
		t.Errorf("bounded end off the contract: %s", got)
	}
}

func TestBoundedEndOmitsReasonWhenServerNamesNone(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){refuse409}}
	err, _, _ := runRetry(t, s, func(o *upOptions) { o.retryFor = 5 * time.Minute })
	if err == nil || strings.Contains(err.Error(), "reason=") || !strings.Contains(err.Error(), " (last answer: no matching offer); nothing was rented") {
		t.Errorf("want no reason on the end: %v", err)
	}
}

func TestRetryForDoesNotClaimNoOfferWhenLastAnswerWasCouldNotLook(t *testing.T) {
	s := &retryServer{script: []func(http.ResponseWriter){couldNotLook}}
	err, _, _ := runRetry(t, s, func(o *upOptions) { o.retryFor = 5 * time.Minute })
	if err == nil || strings.Contains(err.Error(), "no offer matches") {
		t.Fatalf("a could-not-look end must not say no offer matches: %v", err)
	}
}

func TestRetryUntilUpInterruptDuringWaitStopsCleanly(t *testing.T) {
	intr := make(chan os.Signal, 1)
	// The signal lands after the refusal is answered, so it hits the wait and
	// never races the in-flight request (a pre-loaded signal won that race at random).
	s := &retryServer{script: []func(http.ResponseWriter){func(w http.ResponseWriter) {
		refuse409(w)
		go func() { time.Sleep(200 * time.Millisecond); intr <- os.Interrupt }()
	}}}
	err, out, calls := runRetry(t, s, func(o *upOptions) { o.retryGap = time.Hour; o.interrupt = intr })
	if err == nil || calls != 1 || !strings.Contains(out, "Nothing was rented") {
		t.Fatalf("want one POST then a clean stop message: calls=%d err=%v\n%s", calls, err, out)
	}
}

func TestRetryUntilUpInterruptMidPostSaysABoxMayExist(t *testing.T) {
	release := make(chan struct{})
	s := &retryServer{script: []func(http.ResponseWriter){func(w http.ResponseWriter) { <-release; rented(w) }}}
	intr := make(chan os.Signal, 1)
	intr <- os.Interrupt
	err, out, _ := runRetry(t, s, func(o *upOptions) { o.interrupt = intr })
	close(release)
	if err == nil || !strings.Contains(out, "A box may have been created") {
		t.Fatalf("want a message naming the in-flight request: err=%v\n%s", err, out)
	}
}

func TestValidateRetryFlags(t *testing.T) {
	parse := func(args ...string) *flag.FlagSet {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		fs.Bool("retry-until-up", false, "")
		fs.Duration("retry-for", 0, "")
		_ = fs.Parse(args)
		return fs
	}
	if err := validateRetryFlags(parse(), false, 0, true, false); err != nil {
		t.Errorf("no retry flags is always fine: %v", err)
	}
	if err := validateRetryFlags(parse("--retry-until-up"), true, 0, true, false); err == nil || !strings.Contains(err.Error(), "--when-available") {
		t.Errorf("must refuse --when-available: %v", err)
	}
	if err := validateRetryFlags(parse("--retry-for=5m"), false, 5*time.Minute, false, false); err == nil {
		t.Errorf("--retry-for alone must be refused")
	}
	if err := validateRetryFlags(parse("--retry-until-up", "--retry-for=0s"), true, 0, false, false); err == nil {
		t.Errorf("a zero --retry-for must be refused")
	}
	if err := validateRetryFlags(parse("--retry-until-up", "--retry-for=5m"), true, 5*time.Minute, false, false); err != nil {
		t.Errorf("valid combo refused: %v", err)
	}
}
