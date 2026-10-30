package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

func watchRow(id int, status string, extra map[string]any) map[string]any {
	row := map[string]any{
		"id": id, "userId": "u1", "teamId": "team-1",
		"gpuModel": "H100", "gpuCount": 1,
		"maxPricePerHour": nil, "providerScope": nil, "tier": nil,
		"status": status, "expiresAt": "2026-11-02T10:00:00.000Z",
		"notifiedAt": nil, "matchedProvider": nil, "matchedPricePerHour": nil,
		"createdAt": "2026-10-30T10:00:00.000Z", "resolvedAt": nil,
	}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

func feedOffer(gpu, provider string, count int, price float64) map[string]any {
	return map[string]any{
		"address": provider + "/" + gpu, "gpuShortName": gpu, "gpuCount": count,
		"price": price, "provider": provider, "region": "US", "location_id": "loc-1",
		"availableCpu": 8, "availableMemory": map[string]any{"value": 64, "unit": "GB"},
		"availableStorage": map[string]any{"value": 200, "unit": "GB"}, "available": 0,
	}
}

// watchServer fakes the orchestrator for `aq watch`. polls decides the answer
// to the Nth (1-based) GET /availability-watches/:id.
type watchServer struct {
	feed       []map[string]any
	feedStatus int
	armBody    []byte
	armStatus  int
	armMessage string
	polls      func(n int) (status int, row map[string]any)
	pollCount  int32
	deleted    string
	deleteCode int
}

func (s *watchServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/marketplace", func(w http.ResponseWriter, r *http.Request) {
		if s.feedStatus != 0 {
			writeError(w, s.feedStatus, "feed down")
			return
		}
		writeData(w, s.feed)
	})
	mux.HandleFunc("/availability-watches", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			s.armBody, _ = readAll(r)
			if s.armStatus != 0 {
				writeError(w, s.armStatus, s.armMessage)
				return
			}
			var body map[string]any
			_ = json.Unmarshal(s.armBody, &body)
			w.WriteHeader(http.StatusCreated)
			writeData(w, watchRow(7, "WATCHING", map[string]any{"gpuModel": body["gpuModel"], "gpuCount": body["gpuCount"]}))
		case http.MethodGet:
			writeData(w, []map[string]any{watchRow(7, "WATCHING", map[string]any{"maxPricePerHour": 3.5, "providerScope": "runpod"})})
		}
	})
	mux.HandleFunc("/availability-watches/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			s.deleted = r.URL.Path
			if s.deleteCode != 0 {
				writeError(w, s.deleteCode, "Notification not found")
				return
			}
			writeData(w, map[string]any{"status": "CANCELLED"})
			return
		}
		n := int(atomic.AddInt32(&s.pollCount, 1))
		status, row := s.polls(n)
		if status != http.StatusOK {
			writeError(w, status, "boom")
			return
		}
		writeData(w, row)
	})
	return mux
}

func newWatchOpts(url string) watchOptions {
	return watchOptions{
		cred:         &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: url},
		gpuModel:     "H100",
		gpuCount:     1,
		pollInterval: 5 * time.Millisecond,
		out:          &bytes.Buffer{},
		errOut:       &bytes.Buffer{},
	}
}

func exitCodeOf(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err == nil {
		return 0
	}
	return exitFailure
}

func TestWatchArmOmitsUnsetOptionalKeysOnTheWire(t *testing.T) {
	s := &watchServer{feed: []map[string]any{feedOffer("H100", "runpod", 1, 2.5)}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	if err := runWatch(opts); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(s.armBody, &body); err != nil {
		t.Fatalf("armed body is not JSON: %v", err)
	}
	if len(body) != 2 || body["gpuModel"] != "H100" || body["gpuCount"] != float64(1) {
		t.Errorf("a bare watch must send exactly gpuModel and gpuCount, got %s", s.armBody)
	}
	for _, k := range []string{"maxPricePerHour", "providerScope", "tier", "expiresInHours"} {
		if strings.Contains(string(s.armBody), `"`+k+`"`) {
			t.Errorf("unset %s must be omitted from the body, got %s", k, s.armBody)
		}
	}
	errOut := opts.errOut.(*bytes.Buffer).String()
	if !strings.Contains(errOut, "watch 7 armed") || !strings.Contains(errOut, "aq watch wait 7") {
		t.Errorf("arming must print the id and the reattach command on stderr, got %q", errOut)
	}
	if got := opts.out.(*bytes.Buffer).String(); !strings.Contains(got, "Watch 7 armed") {
		t.Errorf("stdout should summarise the watch, got %q", got)
	}
}

func TestWatchArmSendsEveryOptionalWhenSet(t *testing.T) {
	s := &watchServer{feed: []map[string]any{feedOffer("H100", "runpod", 2, 5)}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	price, hours := 3.25, 24.0
	opts := newWatchOpts(srv.URL)
	opts.gpuCount, opts.provider, opts.tier, opts.maxPrice, opts.expiresIn = 2, "runpod", "datacenter", &price, &hours
	if err := runWatch(opts); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(s.armBody, &body)
	want := map[string]any{"gpuModel": "H100", "gpuCount": float64(2), "providerScope": "runpod", "tier": "datacenter", "maxPricePerHour": 3.25, "expiresInHours": float64(24)}
	if len(body) != len(want) {
		t.Fatalf("body = %s", s.armBody)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
}

func TestWatchCorrectsModelSpellingFromTheFeed(t *testing.T) {
	s := &watchServer{feed: []map[string]any{feedOffer("RTX 4090", "vastai", 1, 0.4)}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.gpuModel = "rtx 4090"
	if err := runWatch(opts); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(s.armBody, &body)
	if body["gpuModel"] != "RTX 4090" {
		t.Errorf("gpuModel = %v, want the feed's spelling \"RTX 4090\"", body["gpuModel"])
	}
	if !strings.Contains(opts.errOut.(*bytes.Buffer).String(), `"RTX 4090"`) {
		t.Errorf("the correction should be announced on stderr")
	}
}

func TestWatchWarnsButStillArmsWhenTheModelIsNotInTheFeed(t *testing.T) {
	s := &watchServer{feed: []map[string]any{feedOffer("RTX 4090", "vastai", 1, 0.4)}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.gpuModel = "4090"
	if err := runWatch(opts); err != nil {
		t.Fatalf("a model missing from the feed must still arm, got %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(s.armBody, &body)
	if body["gpuModel"] != "4090" {
		t.Errorf("an unmatched name is sent as typed, got %v", body["gpuModel"])
	}
	e := opts.errOut.(*bytes.Buffer).String()
	if !strings.Contains(e, "warning") || !strings.Contains(e, "aq gpus") || !strings.Contains(e, "RTX 4090") {
		t.Errorf("warning must name `aq gpus` and the close name, got %q", e)
	}
}

func TestWatchWarnsButStillArmsWhenTheFeedCannotBeRead(t *testing.T) {
	s := &watchServer{feedStatus: 500}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	if err := runWatch(opts); err != nil {
		t.Fatalf("a failed feed read must not stop arming, got %v", err)
	}
	if s.armBody == nil {
		t.Fatal("the watch was never armed")
	}
	if !strings.Contains(opts.errOut.(*bytes.Buffer).String(), "warning") {
		t.Errorf("a failed feed read should warn")
	}
}

func TestWatchArmCapRefusalIsSurfacedVerbatim(t *testing.T) {
	s := &watchServer{feed: []map[string]any{feedOffer("H100", "runpod", 1, 2)}, armStatus: 400, armMessage: "You already have 10 active notifications"}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	err := runWatch(newWatchOpts(srv.URL))
	if err == nil || !strings.Contains(err.Error(), "You already have 10 active notifications") {
		t.Errorf("cap refusal must surface verbatim, got %v", err)
	}
}

func TestWatchJSONStdoutIsExactlyOneObject(t *testing.T) {
	s := &watchServer{feed: []map[string]any{feedOffer("H100", "runpod", 1, 2)}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.jsonOut = true
	if err := runWatch(opts); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	assertSingleJSONObject(t, opts.out.(*bytes.Buffer).String(), func(m map[string]any) {
		if m["id"] != float64(7) || m["status"] != "WATCHING" {
			t.Errorf("row = %v", m)
		}
	})
}

func assertSingleJSONObject(t *testing.T, stdout string, check func(map[string]any)) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%q", err, stdout)
	}
	if dec.More() {
		t.Errorf("stdout carries more than one JSON value: %q", stdout)
	}
	if strings.TrimSpace(stdout) != strings.TrimRight(stdout, "\n") && strings.Count(stdout, "\n") > 1 {
		t.Errorf("stdout has stray lines: %q", stdout)
	}
	check(m)
}

func TestWatchWaitNotifiedExitsZeroAndPrintsTheMatch(t *testing.T) {
	s := &watchServer{
		feed: []map[string]any{feedOffer("H100", "runpod", 1, 2)},
		polls: func(n int) (int, map[string]any) {
			if n < 3 {
				return 200, watchRow(7, "WATCHING", nil)
			}
			return 200, watchRow(7, "NOTIFIED", map[string]any{"matchedProvider": "runpod", "matchedPricePerHour": 2.49})
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.wait = true
	if err := runWatch(opts); err != nil {
		t.Fatalf("NOTIFIED must exit 0, got %v", err)
	}
	out := opts.out.(*bytes.Buffer).String()
	if !strings.Contains(out, "runpod") || !strings.Contains(out, "2.49") {
		t.Errorf("must print the matched provider and price, got %q", out)
	}
	if s.pollCount != 3 {
		t.Errorf("polls = %d, want 3", s.pollCount)
	}
}

func TestWatchWaitJSONOnNotifiedPrintsOnlyTheFinalRow(t *testing.T) {
	s := &watchServer{
		feed: []map[string]any{feedOffer("H100", "runpod", 1, 2)},
		polls: func(n int) (int, map[string]any) {
			return 200, watchRow(7, "NOTIFIED", map[string]any{"matchedProvider": "runpod", "matchedPricePerHour": 2.49})
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.wait, opts.jsonOut = true, true
	if err := runWatch(opts); err != nil {
		t.Fatalf("runWatch: %v", err)
	}
	assertSingleJSONObject(t, opts.out.(*bytes.Buffer).String(), func(m map[string]any) {
		if m["status"] != "NOTIFIED" || m["matchedProvider"] != "runpod" {
			t.Errorf("row = %v", m)
		}
	})
}

func TestWatchWaitEndedWithoutMatchExits13NamingWhich(t *testing.T) {
	for _, status := range []string{"EXPIRED", "CANCELLED"} {
		t.Run(status, func(t *testing.T) {
			s := &watchServer{
				feed:  []map[string]any{feedOffer("H100", "runpod", 1, 2)},
				polls: func(n int) (int, map[string]any) { return 200, watchRow(7, status, nil) },
			}
			srv := httptest.NewServer(s.handler())
			defer srv.Close()

			opts := newWatchOpts(srv.URL)
			opts.wait = true
			err := runWatch(opts)
			if exitCodeOf(err) != 13 {
				t.Fatalf("exit = %d (%v), want 13", exitCodeOf(err), err)
			}
			if !strings.Contains(err.Error(), strings.ToLower(status)) {
				t.Errorf("message must name %s, got %v", status, err)
			}
		})
	}
}

func TestWatchWaitTimeoutExits14AndLeavesTheWatchArmed(t *testing.T) {
	s := &watchServer{
		feed:  []map[string]any{feedOffer("H100", "runpod", 1, 2)},
		polls: func(n int) (int, map[string]any) { return 200, watchRow(7, "WATCHING", nil) },
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.wait, opts.timeout = true, 60*time.Millisecond
	err := runWatch(opts)
	if exitCodeOf(err) != 14 {
		t.Fatalf("exit = %d (%v), want 14", exitCodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "aq watch wait 7") || !strings.Contains(err.Error(), "still WATCHING") {
		t.Errorf("message must say the watch stays armed and how to reattach, got %v", err)
	}
	if s.deleted != "" {
		t.Errorf("a timeout must never cancel the watch, but DELETE hit %s", s.deleted)
	}
}

func TestWatchWaitRidesOutServerErrorsAndSpeaksOncePerStreak(t *testing.T) {
	s := &watchServer{
		feed: []map[string]any{feedOffer("H100", "runpod", 1, 2)},
		polls: func(n int) (int, map[string]any) {
			switch {
			case n <= 4:
				return 503, nil
			case n == 5:
				return 200, watchRow(7, "WATCHING", nil)
			case n <= 7:
				return 500, nil
			}
			return 200, watchRow(7, "NOTIFIED", map[string]any{"matchedProvider": "runpod", "matchedPricePerHour": 1.9})
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.wait = true
	if err := runWatch(opts); err != nil {
		t.Fatalf("5xx must not end the wait, got %v", err)
	}
	e := opts.errOut.(*bytes.Buffer).String()
	if got := strings.Count(e, "cannot reach"); got != 2 {
		t.Errorf("want one failure line per streak (2), got %d in %q", got, e)
	}
	if got := strings.Count(e, "reconnected"); got != 2 {
		t.Errorf("want one recovery line per streak (2), got %d in %q", got, e)
	}
}

func TestWatchWaitSurvivesAnUnreachableServerUntilTimeout(t *testing.T) {
	s := &watchServer{feed: []map[string]any{feedOffer("H100", "runpod", 1, 2)}, polls: func(int) (int, map[string]any) { return 200, nil }}
	srv := httptest.NewServer(s.handler())
	opts := newWatchOpts(srv.URL)
	// Arm against the live server, then wait against a dead one.
	row, err := api.NewAuthed(srv.URL, "t", "team-1").ArmAvailabilityWatch(api.ArmWatchRequest{GPUModel: "H100", GPUCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	dead := api.NewAuthed(srv.URL, "t", "team-1")
	srv.Close()

	err = waitForWatch(watchWaitOptions{
		client: dead, id: row.ID, pollInterval: 5 * time.Millisecond, timeout: 80 * time.Millisecond,
		out: opts.out, errOut: opts.errOut,
	})
	if exitCodeOf(err) != 14 {
		t.Fatalf("a wait failing throughout is bounded by --timeout (14), got %d (%v)", exitCodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("the message must say the state is unknown, got %v", err)
	}
	if got := strings.Count(opts.errOut.(*bytes.Buffer).String(), "cannot reach"); got != 1 {
		t.Errorf("want exactly one failure line, got %d", got)
	}
}

func TestWatchWaitAnswersOn401403And404(t *testing.T) {
	for _, status := range []int{401, 403, 404} {
		s := &watchServer{
			feed:  []map[string]any{feedOffer("H100", "runpod", 1, 2)},
			polls: func(n int) (int, map[string]any) { return status, nil },
		}
		srv := httptest.NewServer(s.handler())

		opts := newWatchOpts(srv.URL)
		opts.wait = true
		err := runWatch(opts)
		srv.Close()
		if err == nil || exitCodeOf(err) != exitFailure {
			t.Errorf("%d must end the wait with exit 1, got %v", status, err)
		}
		if s.pollCount != 1 {
			t.Errorf("%d must not be retried, polls = %d", status, s.pollCount)
		}
	}
}

func TestWatchWaitMidWait404IsARealAnswer(t *testing.T) {
	s := &watchServer{
		feed: []map[string]any{feedOffer("H100", "runpod", 1, 2)},
		polls: func(n int) (int, map[string]any) {
			if n < 3 {
				return 200, watchRow(7, "WATCHING", nil)
			}
			return 404, nil
		},
	}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWatchOpts(srv.URL)
	opts.wait = true
	err := runWatch(opts)
	if err == nil || exitCodeOf(err) != exitFailure || !strings.Contains(err.Error(), "not found") {
		t.Errorf("a 404 mid-wait must exit 1 saying not found, got %v", err)
	}
}

func TestIsTransientWatchError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"network":  {errors.New("dial tcp: refused"), true},
		"500":      {&api.APIError{Status: 500}, true},
		"503":      {&api.APIError{Status: 503}, true},
		"429":      {&api.APIError{Status: 429}, true},
		"401":      {&api.APIError{Status: 401}, false},
		"403":      {&api.APIError{Status: 403}, false},
		"404":      {&api.APIError{Status: 404}, false},
		"400":      {&api.APIError{Status: 400}, false},
		"wrapped5": {errors.Join(errors.New("x"), &api.APIError{Status: 502}), true},
	}
	for name, c := range cases {
		if got := isTransientWatchError(c.err); got != c.want {
			t.Errorf("%s: transient = %v, want %v", name, got, c.want)
		}
	}
}

func TestValidateWatchOptions(t *testing.T) {
	neg, zero := -1.0, 0.0
	cases := map[string]struct {
		mut  func(*watchOptions)
		want string
	}{
		"no gpu":           {func(o *watchOptions) { o.gpuModel = "" }, "--gpu is required"},
		"zero gpus":        {func(o *watchOptions) { o.gpuCount = 0 }, "--gpus"},
		"too many gpus":    {func(o *watchOptions) { o.gpuCount = 9 }, "--gpus"},
		"bad tier":         {func(o *watchOptions) { o.tier = "premium" }, "--tier"},
		"zero max price":   {func(o *watchOptions) { o.maxPrice = &zero }, "--max-price"},
		"negative expires": {func(o *watchOptions) { o.expiresIn = &neg }, "--expires-in"},
		"timeout, no wait": {func(o *watchOptions) { o.timeout = time.Minute }, "--timeout only applies with --wait"},
		"negative timeout": {func(o *watchOptions) { o.timeout, o.wait = -time.Second, true }, "--timeout"},
	}
	for name, c := range cases {
		o := watchOptions{gpuModel: "H100", gpuCount: 1}
		c.mut(&o)
		err := validateWatchOptions(o)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
	}
	if err := validateWatchOptions(watchOptions{gpuModel: "H100", gpuCount: 1, wait: true, timeout: time.Minute, tier: "community"}); err != nil {
		t.Errorf("valid options refused: %v", err)
	}
}

func TestWatchCancelAndListAgainstTheWire(t *testing.T) {
	s := &watchServer{}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client := api.NewAuthed(srv.URL, "t", "team-1")

	var out bytes.Buffer
	if err := runWatchCancel(client, 7, &out); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if s.deleted != "/availability-watches/7" || !strings.Contains(out.String(), "Cancelled watch 7") {
		t.Errorf("deleted = %q out = %q", s.deleted, out.String())
	}

	s.deleteCode = 404
	err := runWatchCancel(client, 7, &out)
	if err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Errorf("404 cancel should say not found / no longer active, got %v", err)
	}

	out.Reset()
	if err := runWatchList(client, false, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "runpod") || !strings.Contains(out.String(), "3.5") {
		t.Errorf("list table = %q", out.String())
	}
	out.Reset()
	if err := runWatchList(client, true, &out); err != nil {
		t.Fatalf("list json: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0]["id"] != float64(7) {
		t.Errorf("list --json = %q (%v)", out.String(), err)
	}
}

func TestWatchHelpDocumentsEveryFlagAndExitCode(t *testing.T) {
	section := helpSection(t, "watch:")
	fs := flag.NewFlagSet("aq watch", flag.ContinueOnError)
	registerWatchFlags(fs)
	fs.VisitAll(func(fl *flag.Flag) {
		if !documentsFlag(section, fl.Name) {
			t.Errorf("aq watch accepts --%s but the top-level help's watch: section never names it", fl.Name)
		}
	})
	exitSection := usageText[strings.Index(usageText, "\nExit codes:\n"):]
	for _, code := range []string{"\n  13 ", "\n  14 "} {
		if !strings.Contains(exitSection, code) {
			t.Errorf("Exit codes: section is missing %q", strings.TrimSpace(code))
		}
	}
	if exitWatchNoMatch != 13 || exitWatchTimeout != 14 {
		t.Errorf("exit codes drifted from the documented 13/14")
	}
}
