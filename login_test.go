package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aquanodeio/aq/internal/config"
)

// deviceServer is a minimal fake of the orchestrator device-grant endpoints.
type deviceServer struct {
	approveAfter int32 // polls before flipping to approved
	finalStatus  string
	polls        int32
	lastClient   string
}

func (d *deviceServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api-keys/device/start", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ClientName string `json:"clientName"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.lastClient = body.ClientName
		writeData(w, map[string]any{
			"deviceCode":              "dev-secret-123",
			"userCode":                "WDJB-MFXK",
			"scopes":                  []string{"full"},
			"verificationUri":         "https://console.test/cli",
			"verificationUriComplete": "https://console.test/cli?code=WDJB-MFXK",
			"interval":                1,
			"expiresIn":               600,
		})
	})
	mux.HandleFunc("/api-keys/device/token", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&d.polls, 1)
		if n <= d.approveAfter {
			writeData(w, map[string]any{"status": "pending"})
			return
		}
		switch d.finalStatus {
		case "approved":
			writeData(w, map[string]any{
				"status":  "approved",
				"token":   "aq_sk_test_token",
				"scopes":  []string{"full"},
				"teamId":  "team-xyz",
				"keyName": "aq CLI · testbox",
			})
		case "denied":
			writeData(w, map[string]any{"status": "denied"})
		case "expired":
			writeData(w, map[string]any{"status": "expired"})
		default:
			writeData(w, map[string]any{"status": d.finalStatus})
		}
	})
	return mux
}

func writeData(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func TestRunLoginApprovedPersistsCredential(t *testing.T) {
	srv := httptest.NewServer((&deviceServer{approveAfter: 2, finalStatus: "approved"}).handler())
	defer srv.Close()

	t.Setenv("AQ_CONFIG_DIR", t.TempDir())

	var out bytes.Buffer
	err := runLogin(loginOptions{
		apiURL:       srv.URL,
		clientName:   "testbox",
		out:          &out,
		openBrowser:  false,
		pollInterval: 5 * time.Millisecond,
		now:          time.Now,
	})
	if err != nil {
		t.Fatalf("runLogin returned error: %v", err)
	}

	// The pairing code + approval URL must be surfaced to the user.
	if !strings.Contains(out.String(), "WDJB-MFXK") {
		t.Errorf("output missing user code; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "https://console.test/cli?code=WDJB-MFXK") {
		t.Errorf("output missing verification URL; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Connected as aq CLI · testbox") {
		t.Errorf("output missing success line; got:\n%s", out.String())
	}

	// The issued token must be persisted to the 0600 credential store.
	cred, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cred == nil || cred.Token != "aq_sk_test_token" {
		t.Fatalf("credential not saved correctly: %+v", cred)
	}
	if cred.TeamID != "team-xyz" || cred.APIURL != srv.URL {
		t.Errorf("credential fields wrong: %+v", cred)
	}
}

// TestRunLoginHonorsServerInterval proves the poll loop adopts a larger
// server-advertised interval and treats `slow_down` as backoff, not a failure.
func TestRunLoginHonorsServerInterval(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api-keys/device/start", func(w http.ResponseWriter, r *http.Request) {
		writeData(w, map[string]any{
			"deviceCode": "dev-secret-123",
			"userCode":   "WDJB-MFXK",
			"interval":   2, // server's initial cadence
			"expiresIn":  600,
		})
	})
	var polls int32
	mux.HandleFunc("/api-keys/device/token", func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&polls, 1) {
		case 1:
			// Ask the CLI to slow down and widen to 7s.
			writeData(w, map[string]any{"status": "slow_down", "interval": 7})
		case 2:
			// Advertise a larger cadence on a normal pending poll.
			writeData(w, map[string]any{"status": "pending", "interval": 9})
		default:
			writeData(w, map[string]any{
				"status": "approved", "token": "aq_sk_test_token",
				"teamId": "team-xyz", "keyName": "aq CLI · testbox",
			})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())

	// Record the cadence the loop sleeps at instead of actually waiting.
	var slept []time.Duration
	err := runLogin(loginOptions{
		apiURL: srv.URL,
		out:    &bytes.Buffer{},
		now:    time.Now,
		sleep:  func(d time.Duration) { slept = append(slept, d) },
	})
	if err != nil {
		t.Fatalf("slow_down should not be fatal; got: %v", err)
	}
	cred, _ := config.Load()
	if cred == nil || cred.Token != "aq_sk_test_token" {
		t.Fatalf("credential not saved after backoff: %+v", cred)
	}
	// Sleep #1 uses the start cadence (2s); after slow_down it must widen by at
	// least 5s and adopt the larger advertised 9s — strictly non-decreasing.
	if len(slept) < 3 {
		t.Fatalf("expected at least 3 polls, slept=%v", slept)
	}
	if slept[0] != 2*time.Second {
		t.Errorf("first poll should use server interval 2s, got %v", slept[0])
	}
	for i := 1; i < len(slept); i++ {
		if slept[i] < slept[i-1] {
			t.Errorf("interval shrank: %v then %v", slept[i-1], slept[i])
		}
	}
	if last := slept[len(slept)-1]; last < 9*time.Second {
		t.Errorf("final interval should honor advertised 9s, got %v", last)
	}
}

func TestRunLoginDenied(t *testing.T) {
	srv := httptest.NewServer((&deviceServer{approveAfter: 0, finalStatus: "denied"}).handler())
	defer srv.Close()
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())

	err := runLogin(loginOptions{
		apiURL:       srv.URL,
		out:          &bytes.Buffer{},
		pollInterval: 5 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected denied error, got: %v", err)
	}
	// No credential should be written on denial.
	if cred, _ := config.Load(); cred != nil {
		t.Errorf("credential should not be saved on denial, got: %+v", cred)
	}
}

func TestRunLoginTimeout(t *testing.T) {
	srv := httptest.NewServer((&deviceServer{approveAfter: 1000, finalStatus: "approved"}).handler())
	defer srv.Close()
	t.Setenv("AQ_CONFIG_DIR", t.TempDir())

	// now() jumps past the deadline immediately so the loop exits on timeout.
	calls := 0
	clock := func() time.Time {
		calls++
		if calls > 1 {
			return time.Now().Add(2 * time.Hour)
		}
		return time.Now()
	}
	err := runLogin(loginOptions{
		apiURL:       srv.URL,
		out:          &bytes.Buffer{},
		pollInterval: 5 * time.Millisecond,
		now:          clock,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}
