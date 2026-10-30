package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Aquanodeio/aq/internal/api"
	"github.com/Aquanodeio/aq/internal/config"
)

type queueServer struct {
	feed       []map[string]any
	keyPosts   int
	queueBody  []byte
	queuePosts int
	queueCode  int
	queueMsg   string
	deleted    string
	deleteCode int
}

func (s *queueServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/marketplace", func(w http.ResponseWriter, r *http.Request) { writeData(w, s.feed) })
	mux.HandleFunc("/settings/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeData(w, []map[string]any{})
			return
		}
		s.keyPosts++
		writeData(w, map[string]any{"id": "key-1", "name": "n", "public_key": "k"})
	})
	mux.HandleFunc("/queued-deployments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeData(w, []map[string]any{{
				"id": 3, "gpuModel": "H100", "gpuCount": 2, "priceCeilingPerHour": 4.5,
				"providerScope": nil, "status": "QUEUED", "expiresAt": "2026-11-02T10:00:00.000Z",
				"deploymentId": nil, "lastError": nil, "createdAt": "2026-10-30T10:00:00.000Z",
			}})
			return
		}
		s.queuePosts++
		s.queueBody, _ = readAll(r)
		if s.queueCode != 0 {
			writeError(w, s.queueCode, s.queueMsg)
			return
		}
		var b map[string]any
		_ = json.Unmarshal(s.queueBody, &b)
		w.WriteHeader(http.StatusCreated)
		writeData(w, map[string]any{
			"id": 3, "gpuModel": b["gpuModel"], "gpuCount": b["gpuCount"], "priceCeilingPerHour": b["priceCeilingPerHour"],
			"providerScope": nil, "status": "QUEUED", "expiresAt": "2026-11-02T10:00:00.000Z",
			"deploymentId": nil, "lastError": nil, "createdAt": "2026-10-30T10:00:00.000Z",
		})
	})
	mux.HandleFunc("/queued-deployments/", func(w http.ResponseWriter, r *http.Request) {
		s.deleted = r.URL.Path
		if s.deleteCode != 0 {
			writeError(w, s.deleteCode, "not found")
			return
		}
		writeData(w, map[string]any{"status": "CANCELLED"})
	})
	return mux
}

func soldOut(gpu, provider string, count int, price float64) map[string]any {
	o := feedOffer(gpu, provider, count, price)
	o["available"] = 0
	return o
}

func newWhenOpts(url string) whenAvailableOptions {
	return whenAvailableOptions{
		cred:     &config.Credential{Token: "aq_sk_test", TeamID: "team-1", APIURL: url},
		gpuModel: "H100", gpuCount: 1, maxPrice: 3,
		out: &bytes.Buffer{}, errOut: &bytes.Buffer{},
	}
}

func TestWhenAvailableQueuesASoldOutOfferAboveTheCeiling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@x")
	// The only row is sold out AND priced above the ceiling: neither may refuse.
	s := &queueServer{feed: []map[string]any{soldOut("H100", "runpod", 1, 9.99)}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	opts := newWhenOpts(srv.URL)
	opts.gpuModel = "h100"
	if err := runUpWhenAvailable(opts); err != nil {
		t.Fatalf("runUpWhenAvailable: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(s.queueBody, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 4 {
		t.Errorf("top-level keys must be exactly gpuModel, gpuCount, priceCeilingPerHour, request; got %s", s.queueBody)
	}
	if body["gpuModel"] != "H100" || body["gpuCount"] != float64(1) || body["priceCeilingPerHour"] != float64(3) {
		t.Errorf("body = %s", s.queueBody)
	}
	for _, k := range []string{"providerScope", "expiresInHours", "tier"} {
		if _, ok := body[k]; ok {
			t.Errorf("unset %s must be absent, got %s", k, s.queueBody)
		}
	}
	req := body["request"].(map[string]any)
	if len(req) != 3 {
		t.Errorf("request keys = %v, want resource, provider, serviceConfig", req)
	}
	svc := req["serviceConfig"].(map[string]any)
	want := map[string]any{"type": "VMS", "vmSlug": "ubuntu-24-vm", "sshKeyId": "key-1", "snapshotterConfig": "aquanode"}
	if len(svc) != len(want) {
		t.Errorf("serviceConfig = %v", svc)
	}
	for k, v := range want {
		if svc[k] != v {
			t.Errorf("serviceConfig.%s = %v, want %v", k, svc[k], v)
		}
	}
	if req["provider"].(map[string]any)["name"] != "runpod" {
		t.Errorf("provider = %v", req["provider"])
	}
	res := req["resource"].(map[string]any)
	if _, ok := res["name"]; ok {
		t.Errorf("an unset --name must be absent from resource, got %v", res)
	}
	if res["gpuModel"] != "H100" || res["gpuUnits"] != float64(1) || res["desiredInstanceId"] != "runpod/H100" {
		t.Errorf("resource = %v", res)
	}
	out := opts.out.(*bytes.Buffer).String()
	for _, w := range []string{"Queued #3", "3.00", "aq queue cancel 3", "Expires"} {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q: %q", w, out)
		}
	}
}

func TestWhenAvailableSendsOptionalsAndNameWhenGiven(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@x")
	s := &queueServer{feed: []map[string]any{
		soldOut("H100", "runpod", 1, 3), soldOut("H100", "vastai", 2, 5), soldOut("H100", "vastai", 1, 2),
	}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	hours := 48.0
	opts := newWhenOpts(srv.URL)
	opts.gpuCount, opts.provider, opts.name, opts.expiresIn = 2, "VastAI", " my-box ", &hours
	if err := runUpWhenAvailable(opts); err != nil {
		t.Fatalf("runUpWhenAvailable: %v", err)
	}
	var body map[string]any
	_ = json.Unmarshal(s.queueBody, &body)
	if body["providerScope"] != "vastai" || body["expiresInHours"] != float64(48) || body["gpuCount"] != float64(2) {
		t.Errorf("body = %s", s.queueBody)
	}
	res := body["request"].(map[string]any)["resource"].(map[string]any)
	if res["name"] != "my-box" || res["gpuUnits"] != float64(2) {
		t.Errorf("resource = %v", res)
	}
	// The 2-GPU vastai row is the template, not the cheaper 1-GPU one.
	if res["desiredInstanceId"] != "vastai/H100" {
		t.Errorf("resource = %v", res)
	}
}

func TestWhenAvailableRefusesWhenTheFeedHasNoRowAndRegistersNoKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@x")
	s := &queueServer{feed: []map[string]any{soldOut("RTX 4090", "vastai", 1, 0.4)}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	for name, mut := range map[string]func(*whenAvailableOptions){
		"unknown model":    func(o *whenAvailableOptions) { o.gpuModel = "4090" },
		"unlisted vendor":  func(o *whenAvailableOptions) { o.gpuModel = "RTX 4090"; o.provider = "runpod" },
		"a model, no name": func(o *whenAvailableOptions) { o.gpuModel = "H200" },
	} {
		opts := newWhenOpts(srv.URL)
		mut(&opts)
		err := runUpWhenAvailable(opts)
		if err == nil || !strings.Contains(err.Error(), "nothing to base the request on") {
			t.Errorf("%s: want a no-row refusal, got %v", name, err)
		}
	}
	if s.queuePosts != 0 || s.keyPosts != 0 {
		t.Errorf("a refusal must POST nothing: queue=%d key=%d", s.queuePosts, s.keyPosts)
	}
}

func TestWhenAvailableCapRefusalIsSurfacedVerbatim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeFakePubKey(t, "ssh-ed25519 AAAA laptop@x")
	s := &queueServer{feed: []map[string]any{soldOut("H100", "runpod", 1, 3)}, queueCode: 400, queueMsg: "You can have at most 3 queued requests"}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	err := runUpWhenAvailable(newWhenOpts(srv.URL))
	if err == nil || !strings.Contains(err.Error(), "You can have at most 3 queued requests") {
		t.Errorf("got %v", err)
	}
}

// Every refusal happens before a credential is even read, so up() can be
// driven directly with no server and no login.
func TestUpWhenAvailableRefusalsHappenBeforeAnyNetworkCall(t *testing.T) {
	cases := map[string]struct {
		args []string
		want string
	}{
		"no gpu":           {[]string{"--when-available", "--max-price", "3"}, "--gpu"},
		"no max price":     {[]string{"--when-available", "--gpu", "H100"}, "mandatory spend rail"},
		"zero ceiling":     {[]string{"--when-available", "--gpu", "H100", "--max-price", "0"}, "mandatory spend rail"},
		"comfyui":          {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--comfyui"}, "--comfyui"},
		"jupyter":          {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--jupyter"}, "--jupyter"},
		"auto-stop":        {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--auto-stop"}, "--auto-stop"},
		"warn-after":       {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--warn-after", "30m"}, "--warn-after"},
		"stop-after":       {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--stop-after", "1h"}, "--stop-after"},
		"show-secrets":     {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--show-secrets"}, "--show-secrets"},
		"host target":      {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "host:lease-a"}, "host:lease-a"},
		"other target":     {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "box"}, "no deployment argument"},
		"too many gpus":    {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--gpus", "9"}, "--gpus"},
		"bad expiry":       {[]string{"--when-available", "--gpu", "H100", "--max-price", "3", "--expires-in", "-4"}, "--expires-in"},
		"expires-in alone": {[]string{"--gpu", "H100", "--expires-in", "4"}, "only applies with --when-available"},
	}
	for name, c := range cases {
		err := up(c.args)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "not logged in") {
			t.Errorf("%s: refused only after reading the credential", name)
		}
	}
}

// `aq up --when-available` rents unattended, so it must ride up's billable
// guard untouched: refused from a non-interactive shell against a remote host
// unless --prod / AQ_ALLOW_PROD says otherwise.
func TestWhenAvailableStaysBehindTheBillableGuard(t *testing.T) {
	args := []string{"--when-available", "--gpu", "H100", "--max-price", "3"}
	prod := "https://server.aquanode.io/api/v1"

	err := guardBillable(podsGuardCmd("up", args), prod, args, false, false)
	if err == nil || !strings.Contains(err.Error(), "--prod") {
		t.Fatalf("--when-available from a non-interactive shell must be refused without --prod, got %v", err)
	}
	// --prod is consumed by main before up() parses its flags, so the flag set
	// never sees it, and the opt-in lets the request through.
	stripped, allow := stripProdFlag(append([]string{"--prod"}, args...))
	if !allow || strings.Join(stripped, " ") != strings.Join(args, " ") {
		t.Fatalf("stripProdFlag = %v %v", stripped, allow)
	}
	if err := guardBillable("up", prod, stripped, allow, false); err != nil {
		t.Errorf("--prod must allow it: %v", err)
	}
	if err := guardBillable("up", "http://localhost:3080/api/v1", args, false, false); err != nil {
		t.Errorf("a local stack is never guarded: %v", err)
	}
	// watch and queue spend nothing and stay out of the billable set.
	for _, cmd := range []string{"watch", "queue"} {
		if _, billable := billableCommands[cmd]; billable {
			t.Errorf("aq %s rents nothing and must not be in billableCommands", cmd)
		}
		var buf bytes.Buffer
		announceTarget(cmd, prod, &buf)
		if !strings.Contains(buf.String(), prod) {
			t.Errorf("aq %s changes state and must announce its host", cmd)
		}
	}
}

func TestPickQueueTemplatePrefersTheRequestedCountThenFallsBack(t *testing.T) {
	offers := []api.MarketplaceOffer{
		{GPUShortName: "H100", Provider: "runpod", GPUCount: 1, Price: 2},
		{GPUShortName: "H100", Provider: "runpod", GPUCount: 4, Price: 9},
		{GPUShortName: "H100 NVL", Provider: "runpod", GPUCount: 4, Price: 1},
	}
	o, err := pickQueueTemplate(offers, "h100", 4, "")
	if err != nil || o.Price != 9 {
		t.Errorf("want the exact-name 4x row, got %+v %v", o, err)
	}
	o, err = pickQueueTemplate(offers, "H100", 8, "")
	if err != nil || o.Price != 2 {
		t.Errorf("no 8x row: want the cheapest H100 stand-in, got %+v %v", o, err)
	}
}

func TestQueueListAndCancelAgainstTheWire(t *testing.T) {
	s := &queueServer{}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client := api.NewAuthed(srv.URL, "t", "team-1")

	var out bytes.Buffer
	if err := runQueueList(client, false, &out); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"ID", "H100", "any", "4.5"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("list missing %q: %q", w, out.String())
		}
	}
	out.Reset()
	if err := runQueueList(client, true, &out); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0]["status"] != "QUEUED" {
		t.Errorf("list --json = %q (%v)", out.String(), err)
	}

	out.Reset()
	if err := runQueueCancel(client, 3, &out); err != nil || s.deleted != "/queued-deployments/3" {
		t.Errorf("cancel: %v deleted=%q", err, s.deleted)
	}
	s.deleteCode = 404
	if err := runQueueCancel(client, 3, &out); err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Errorf("404 cancel = %v", err)
	}
}

func TestUpHelpDocumentsWhenAvailable(t *testing.T) {
	for _, flagName := range []string{"when-available", "expires-in"} {
		if !documentsFlag(usageText, flagName) {
			t.Errorf("top-level help never names --%s", flagName)
		}
	}
	if !documentsFlag(helpSection(t, "queue:"), "json") {
		t.Errorf("queue: section should document --json")
	}
}
