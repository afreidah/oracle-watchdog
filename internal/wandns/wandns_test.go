// -------------------------------------------------------------------------------
// Oracle Watchdog - WAN-IP DDNS Updater Tests
//
// Project: Munchbox / Author: Alex Freidah
// -------------------------------------------------------------------------------

package wandns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/afreidah/oracle-watchdog/internal/config"
)

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// baseConfig returns a valid WanDNSConfig with placeholder values.
func baseConfig(provider string) config.WanDNSConfig {
	return config.WanDNSConfig{
		Enabled:  true,
		Hostname: "wg.example.com",
		Cloudflare: config.CloudflareConfig{
			APITokenEnv: "CLOUDFLARE_API_TOKEN_TEST",
			ZoneID:      "zone-abc",
		},
		DetectionProviders: []string{provider},
		PollInterval:       time.Minute,
		Cooldown:           time.Minute,
	}
}

// newUpdater returns an Updater wired to the supplied test server URL with
// the token shortcut so tests do not need to mutate environment variables.
func newUpdater(t *testing.T, cfg config.WanDNSConfig, baseURL string) *Updater {
	t.Helper()
	u, err := New(cfg, WithBaseURL(baseURL), WithToken("test-token"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return u
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// TestNew_RequiresToken verifies New surfaces ErrTokenMissing when neither
// the env var nor WithToken supplies a credential.
func TestNew_RequiresToken(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN_MISSING_FOR_TEST", "")
	cfg := baseConfig("https://api.example.com/ip")
	cfg.Cloudflare.APITokenEnv = "CLOUDFLARE_API_TOKEN_MISSING_FOR_TEST"
	if _, err := New(cfg); !errors.Is(err, ErrTokenMissing) {
		t.Fatalf("expected ErrTokenMissing, got %v", err)
	}
}

// TestNew_TokenFromEnv verifies the env var path is consulted when WithToken
// is not used.
func TestNew_TokenFromEnv(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN_TEST_OK", "from-env")
	cfg := baseConfig("https://api.example.com/ip")
	cfg.Cloudflare.APITokenEnv = "CLOUDFLARE_API_TOKEN_TEST_OK"
	u, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if u.token != "from-env" {
		t.Errorf("expected token from env, got %q", u.token)
	}
}

// -------------------------------------------------------------------------
// DETECTION
// -------------------------------------------------------------------------

// TestDetect_PlainIPify covers the canonical ipify.org response shape
// (single IP, plain text, no headers).
func TestDetect_PlainIPify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.42")
	}))
	defer srv.Close()

	cfg := baseConfig(srv.URL)
	u := newUpdater(t, cfg, "http://unused")
	ip, err := u.detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if ip != "203.0.113.42" {
		t.Errorf("expected 203.0.113.42, got %s", ip)
	}
}

// TestDetect_CloudflareTrace covers the multi-line key=value response shape
// from 1.1.1.1/cdn-cgi/trace.
func TestDetect_CloudflareTrace(t *testing.T) {
	body := "fl=99x99\nh=1.1.1.1\nip=198.51.100.7\nts=12345.67\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	cfg := baseConfig(srv.URL)
	u := newUpdater(t, cfg, "http://unused")
	ip, err := u.detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if ip != "198.51.100.7" {
		t.Errorf("expected 198.51.100.7, got %s", ip)
	}
}

// TestDetect_FailoverToSecondProvider verifies a failed primary provider
// causes the secondary to be tried instead of bubbling the failure up.
func TestDetect_FailoverToSecondProvider(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.99")
	}))
	defer good.Close()

	cfg := baseConfig(bad.URL)
	cfg.DetectionProviders = []string{bad.URL, good.URL}
	u := newUpdater(t, cfg, "http://unused")

	ip, err := u.detect(context.Background())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if ip != "203.0.113.99" {
		t.Errorf("expected 203.0.113.99, got %s", ip)
	}
}

// TestDetect_AllProvidersFail verifies the sentinel error is returned when
// every provider fails.
func TestDetect_AllProvidersFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	cfg := baseConfig(bad.URL)
	cfg.DetectionProviders = []string{bad.URL, bad.URL}
	u := newUpdater(t, cfg, "http://unused")

	if _, err := u.detect(context.Background()); !errors.Is(err, ErrAllProvidersFailed) {
		t.Fatalf("expected ErrAllProvidersFailed, got %v", err)
	}
}

// TestParseDetectionBody_Variants exercises each branch of the body parser.
func TestParseDetectionBody_Variants(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain ipv4", "203.0.113.5", "203.0.113.5"},
		{"plain ipv4 with whitespace", "  203.0.113.6\n", "203.0.113.6"},
		{"trace style", "fl=1\nip=203.0.113.7\nts=2", "203.0.113.7"},
		{"trace style trailing space", "fl=1\nip=  203.0.113.8 \nts=2", "203.0.113.8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDetectionBody([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestParseDetectionBody_Rejects covers parser failure cases.
func TestParseDetectionBody_Rejects(t *testing.T) {
	cases := []string{
		"",
		"not an ip",
		"ip=not-an-ip\n",
		"fe80::1\n",
	}
	for _, c := range cases {
		if _, err := parseDetectionBody([]byte(c)); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

// -------------------------------------------------------------------------
// CLOUDFLARE
// -------------------------------------------------------------------------

// fakeCFServer routes the two endpoints the updater hits and lets tests
// observe the requests it received.
type fakeCFServer struct {
	t          *testing.T
	listIP     string
	listStatus int
	listFail   bool
	patchOK    bool
	patchFail  bool
	patchCalls atomic.Int32
	lastBody   atomic.Value
}

// handler returns an http.Handler that dispatches based on URL path.
func (s *fakeCFServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			s.t.Errorf("missing or wrong auth header: %q", auth)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/dns_records"):
			s.handleList(w)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/dns_records/"):
			s.handlePatch(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// handleList emits the configured list response.
func (s *fakeCFServer) handleList(w http.ResponseWriter) {
	if s.listStatus != 0 && s.listStatus != http.StatusOK {
		w.WriteHeader(s.listStatus)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"success": !s.listFail,
		"result": []map[string]string{{
			"id":      "rec-1",
			"name":    "wg.example.com",
			"type":    "A",
			"content": s.listIP,
		}},
		"errors": []any{},
	}
	if s.listFail {
		body["result"] = []any{}
		body["errors"] = []map[string]any{{"code": 9999, "message": "bad"}}
	}
	_ = json.NewEncoder(w).Encode(body)
}

// handlePatch records the request body and emits the configured response.
func (s *fakeCFServer) handlePatch(w http.ResponseWriter, r *http.Request) {
	s.patchCalls.Add(1)
	buf := make([]byte, 0, 256)
	if r.ContentLength > 0 {
		tmp := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(tmp)
		buf = append(buf, tmp...)
	}
	s.lastBody.Store(string(buf))

	if s.patchFail {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1234,"message":"oops"}]}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if s.patchOK {
		_, _ = w.Write([]byte(`{"success":true,"errors":[]}`))
		return
	}
	_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1,"message":"unset"}]}`))
}

// TestTick_NoChange verifies a tick where the detected IP matches the
// existing record does not result in a PATCH call.
func TestTick_NoChange(t *testing.T) {
	cf := &fakeCFServer{t: t, listIP: "203.0.113.10", patchOK: true}
	cfSrv := httptest.NewServer(cf.handler())
	defer cfSrv.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.10")
	}))
	defer ipSrv.Close()

	cfg := baseConfig(ipSrv.URL)
	u := newUpdater(t, cfg, cfSrv.URL)
	u.tick(context.Background())

	if cf.patchCalls.Load() != 0 {
		t.Errorf("expected 0 PATCH calls when IP matches, got %d", cf.patchCalls.Load())
	}
}

// TestTick_UpdatesOnChange verifies a tick where the detected IP differs
// triggers a PATCH with the new content.
func TestTick_UpdatesOnChange(t *testing.T) {
	cf := &fakeCFServer{t: t, listIP: "203.0.113.10", patchOK: true}
	cfSrv := httptest.NewServer(cf.handler())
	defer cfSrv.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.20")
	}))
	defer ipSrv.Close()

	cfg := baseConfig(ipSrv.URL)
	u := newUpdater(t, cfg, cfSrv.URL)
	u.tick(context.Background())

	if cf.patchCalls.Load() != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", cf.patchCalls.Load())
	}
	body, _ := cf.lastBody.Load().(string)
	if !strings.Contains(body, `"content":"203.0.113.20"`) {
		t.Errorf("expected new IP in body, got %q", body)
	}
}

// TestTick_DetectionFailureSkipsCloudflare verifies that when no provider
// returns an IP the Cloudflare API is not consulted.
func TestTick_DetectionFailureSkipsCloudflare(t *testing.T) {
	cf := &fakeCFServer{t: t, listIP: "203.0.113.10", patchOK: true}
	cfSrv := httptest.NewServer(cf.handler())
	defer cfSrv.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ipSrv.Close()

	cfg := baseConfig(ipSrv.URL)
	u := newUpdater(t, cfg, cfSrv.URL)
	u.tick(context.Background())

	if cf.patchCalls.Load() != 0 {
		t.Errorf("expected 0 PATCH calls on detection failure, got %d", cf.patchCalls.Load())
	}
}

// TestTick_CloudflareUpdateFailureIsRecoverable verifies that a 4xx from
// Cloudflare during PATCH does not crash the updater.
func TestTick_CloudflareUpdateFailureIsRecoverable(t *testing.T) {
	cf := &fakeCFServer{t: t, listIP: "203.0.113.10", patchFail: true}
	cfSrv := httptest.NewServer(cf.handler())
	defer cfSrv.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.20")
	}))
	defer ipSrv.Close()

	cfg := baseConfig(ipSrv.URL)
	u := newUpdater(t, cfg, cfSrv.URL)
	u.tick(context.Background())

	if cf.patchCalls.Load() != 1 {
		t.Errorf("expected 1 PATCH attempt, got %d", cf.patchCalls.Load())
	}
}

// TestTick_CloudflareLookupFailureIsRecoverable verifies a Cloudflare GET
// failure does not panic and skips the update.
func TestTick_CloudflareLookupFailureIsRecoverable(t *testing.T) {
	cf := &fakeCFServer{t: t, listStatus: http.StatusInternalServerError}
	cfSrv := httptest.NewServer(cf.handler())
	defer cfSrv.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.20")
	}))
	defer ipSrv.Close()

	cfg := baseConfig(ipSrv.URL)
	u := newUpdater(t, cfg, cfSrv.URL)
	u.tick(context.Background())

	if cf.patchCalls.Load() != 0 {
		t.Errorf("expected 0 PATCH calls when lookup fails, got %d", cf.patchCalls.Load())
	}
}

// TestTick_CooldownBlocksUpdate verifies that a recent successful update
// suppresses subsequent attempts within the cooldown window.
func TestTick_CooldownBlocksUpdate(t *testing.T) {
	cf := &fakeCFServer{t: t, listIP: "203.0.113.10", patchOK: true}
	cfSrv := httptest.NewServer(cf.handler())
	defer cfSrv.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.20")
	}))
	defer ipSrv.Close()

	cfg := baseConfig(ipSrv.URL)
	cfg.Cooldown = 5 * time.Minute
	u := newUpdater(t, cfg, cfSrv.URL)

	u.tick(context.Background())
	u.tick(context.Background())

	if cf.patchCalls.Load() != 1 {
		t.Errorf("expected exactly 1 PATCH (second blocked by cooldown), got %d", cf.patchCalls.Load())
	}
}

// TestRun_HonoursContextCancellation verifies Run returns promptly after
// ctx cancellation.
func TestRun_HonoursContextCancellation(t *testing.T) {
	cf := &fakeCFServer{t: t, listIP: "203.0.113.10", patchOK: true}
	cfSrv := httptest.NewServer(cf.handler())
	defer cfSrv.Close()
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "203.0.113.10")
	}))
	defer ipSrv.Close()

	cfg := baseConfig(ipSrv.URL)
	cfg.PollInterval = 10 * time.Millisecond
	u := newUpdater(t, cfg, cfSrv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		u.Run(ctx)
		close(done)
	}()

	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of cancel")
	}
}

// TestJoinErrors_FormatsConcisely covers the empty and populated branches.
func TestJoinErrors_FormatsConcisely(t *testing.T) {
	if got := joinErrors(nil); got != "<empty>" {
		t.Errorf("nil errs: got %q want <empty>", got)
	}
	got := joinErrors([]cloudflareError{
		{Code: 1, Message: "a"},
		{Code: 2, Message: "b"},
	})
	if got != "1: a; 2: b" {
		t.Errorf("got %q", got)
	}
}
