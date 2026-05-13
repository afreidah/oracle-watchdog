// -------------------------------------------------------------------------------
// Oracle Watchdog - WAN-IP DDNS Updater
//
// Project: Munchbox / Author: Alex Freidah
//
// Periodically detects the home WAN IPv4 address by polling lightweight
// detection providers and keeps a Cloudflare DNS A record in sync. Solves the
// stale-endpoint problem that arises when a residential ISP rotates the
// public IP without operator intervention.
//
// The Cloudflare API token is read from the environment variable named in the
// configuration so the secret never enters the loaded config struct or appears
// in serialized state.
//
// Self-healing design: never crashes on HTTP, JSON, or Cloudflare errors.
// Continuously retries on the next poll and emits metrics on current state.
// -------------------------------------------------------------------------------

package wandns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/afreidah/oracle-watchdog/internal/config"
	"github.com/afreidah/oracle-watchdog/internal/metrics"
	"github.com/afreidah/oracle-watchdog/internal/tracing"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// cloudflareAPIBase is the production Cloudflare REST API root. Tests override
// it via WithBaseURL so they can point at an httptest.Server.
const cloudflareAPIBase = "https://api.cloudflare.com/client/v4"

// detectTimeout caps each detection-provider HTTP request. Providers should
// answer quickly; treat anything slower as failure.
const detectTimeout = 10 * time.Second

// cloudflareTimeout caps Cloudflare API requests.
const cloudflareTimeout = 15 * time.Second

// -------------------------------------------------------------------------
// SENTINEL ERRORS
// -------------------------------------------------------------------------

// ErrAllProvidersFailed is returned when every configured detection provider
// failed to return a parseable IPv4 address on a single tick.
var ErrAllProvidersFailed = errors.New("all detection providers failed")

// ErrTokenMissing is returned when the configured environment variable does
// not contain a Cloudflare API token at construction time.
var ErrTokenMissing = errors.New("cloudflare api token environment variable is empty")

// -------------------------------------------------------------------------
// HTTP CLIENT INTERFACE
// -------------------------------------------------------------------------

// HTTPDoer narrows the *http.Client surface to the single Do method so tests
// can inject a custom round-tripper without standing up a server.
type HTTPDoer interface {
	// Do executes the request and returns its response.
	Do(req *http.Request) (*http.Response, error)
}

// -------------------------------------------------------------------------
// UPDATER
// -------------------------------------------------------------------------

// Updater polls detection providers for the current WAN IP and updates a
// Cloudflare DNS A record when the value changes.
type Updater struct {
	// cfg is the validated configuration the updater was constructed with.
	cfg config.WanDNSConfig

	// token is the Cloudflare API bearer token captured once at construction
	// from the configured environment variable.
	token string

	// httpClient performs all outbound HTTP calls (detection + Cloudflare).
	httpClient HTTPDoer

	// baseURL is the Cloudflare REST API root. Configurable via WithBaseURL
	// so tests can substitute httptest.Server addresses.
	baseURL string

	// log is the component-scoped logger used by all updater output.
	log *slog.Logger

	// mu guards the cooldown state.
	mu sync.Mutex

	// lastUpdated is the wall-clock time of the most recent successful
	// Cloudflare record update; zero means no update yet.
	lastUpdated time.Time

	// lastIP is the most recent WAN IP applied to Cloudflare so the previous
	// label value can be deleted from the WanIPCurrent gauge on change.
	lastIP string

	// recordIDCache memoizes the Cloudflare record ID for the configured
	// hostname so subsequent updates skip the lookup round-trip.
	recordIDCache string
}

// Option configures the Updater. Used to inject test doubles for the HTTP
// client, the base URL, the logger, and the token source.
type Option func(*Updater)

// WithHTTPClient overrides the default *http.Client.
func WithHTTPClient(c HTTPDoer) Option {
	return func(u *Updater) { u.httpClient = c }
}

// WithBaseURL overrides the Cloudflare API base URL. Used by tests to point
// at an httptest.Server.
func WithBaseURL(url string) Option {
	return func(u *Updater) { u.baseURL = strings.TrimRight(url, "/") }
}

// WithLogger overrides the default scoped logger.
func WithLogger(l *slog.Logger) Option {
	return func(u *Updater) { u.log = l }
}

// WithToken overrides the Cloudflare API token. When supplied, the env var
// named in cfg.Cloudflare.APITokenEnv is not consulted.
func WithToken(token string) Option {
	return func(u *Updater) { u.token = token }
}

// New constructs an Updater from the given config. Reads the Cloudflare token
// from the configured environment variable unless overridden via WithToken so
// missing-token errors surface at construction time rather than first poll.
func New(cfg config.WanDNSConfig, opts ...Option) (*Updater, error) {
	u := &Updater{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cloudflareTimeout},
		baseURL:    cloudflareAPIBase,
		log:        slog.Default().With("component", "wandns"),
	}
	for _, opt := range opts {
		opt(u)
	}
	if u.token == "" {
		u.token = os.Getenv(cfg.Cloudflare.APITokenEnv)
	}
	if u.token == "" {
		return nil, fmt.Errorf("%w: %s", ErrTokenMissing, cfg.Cloudflare.APITokenEnv)
	}
	return u, nil
}

// -------------------------------------------------------------------------
// RUN LOOP
// -------------------------------------------------------------------------

// Run drives the polling loop until ctx is cancelled. Performs an initial
// tick on entry so endpoint changes are picked up promptly after the updater
// starts.
func (u *Updater) Run(ctx context.Context) {
	u.log.InfoContext(ctx, "starting wan dns updater",
		"hostname", u.cfg.Hostname,
		"poll_interval", u.cfg.PollInterval,
		"cooldown", u.cfg.Cooldown,
	)

	u.tick(ctx)

	ticker := time.NewTicker(u.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			u.log.InfoContext(ctx, "stopping wan dns updater")
			return
		case <-ticker.C:
			u.tick(ctx)
		}
	}
}

// -------------------------------------------------------------------------
// TICK
// -------------------------------------------------------------------------

// tick performs one detect-compare-update cycle. Records the cooldown gauge
// and the last-check timestamp regardless of outcome so dashboards can see
// the updater is alive even when no changes occur.
func (u *Updater) tick(ctx context.Context) {
	ctx, span := tracing.StartClientSpan(ctx, "wandns.tick",
		tracing.PeerServiceAttr("cloudflare"),
	)
	defer span.End()

	metrics.WanDNSLastCheck.SetToCurrentTime()

	ip, err := u.detect(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "detection failed")
		u.log.WarnContext(ctx, "wan ip detection failed", "error", err)
		return
	}

	u.recordCurrentIP(ip)

	if u.inCooldown() {
		metrics.WanDNSInCooldown.Set(1)
		span.SetStatus(codes.Ok, "in cooldown")
		return
	}
	metrics.WanDNSInCooldown.Set(0)

	current, recordID, err := u.fetchRecord(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "cloudflare lookup failed")
		u.log.WarnContext(ctx, "cloudflare record lookup failed", "error", err)
		return
	}

	if current == ip {
		span.SetStatus(codes.Ok, "no change")
		return
	}

	if err := u.updateRecord(ctx, recordID, ip); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "cloudflare update failed")
		metrics.CloudflareRecordUpdates.WithLabelValues("fail").Inc()
		u.log.WarnContext(ctx, "cloudflare record update failed",
			"hostname", u.cfg.Hostname,
			"new_ip", ip,
			"error", err,
		)
		return
	}

	metrics.CloudflareRecordUpdates.WithLabelValues("success").Inc()
	metrics.WanIPChanges.Inc()
	u.markUpdated(ip)
	u.log.InfoContext(ctx, "cloudflare record updated",
		"hostname", u.cfg.Hostname,
		"previous_ip", current,
		"new_ip", ip,
	)
	span.SetStatus(codes.Ok, "record updated")
}

// -------------------------------------------------------------------------
// DETECTION
// -------------------------------------------------------------------------

// detect queries each configured provider in order and returns the first
// parseable IPv4 address. Failures per provider are recorded as metrics so
// operators can identify a consistently-bad provider.
func (u *Updater) detect(ctx context.Context) (string, error) {
	for _, p := range u.cfg.DetectionProviders {
		ip, err := u.detectFrom(ctx, p)
		if err != nil {
			metrics.WanIPDetectionFailures.WithLabelValues(p).Inc()
			u.log.DebugContext(ctx, "detection provider failed",
				"provider", p,
				"error", err,
			)
			continue
		}
		return ip, nil
	}
	return "", ErrAllProvidersFailed
}

// detectFrom issues a GET to the provider URL and parses the response into an
// IPv4 string. Supports both plain-text bodies (ipify) and the Cloudflare
// trace key=value format.
func (u *Updater) detectFrom(ctx context.Context, url string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return parseDetectionBody(body)
}

// parseDetectionBody extracts an IPv4 address from a detection-provider
// response. Trims whitespace for plain-text bodies and scans for an "ip="
// line in Cloudflare-style trace responses.
func parseDetectionBody(body []byte) (string, error) {
	text := strings.TrimSpace(string(body))
	if ip := net.ParseIP(text); ip != nil && ip.To4() != nil {
		return ip.To4().String(), nil
	}
	for line := range strings.SplitSeq(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "ip="); ok {
			candidate := strings.TrimSpace(rest)
			if ip := net.ParseIP(candidate); ip != nil && ip.To4() != nil {
				return ip.To4().String(), nil
			}
		}
	}
	return "", fmt.Errorf("no ipv4 in body")
}

// -------------------------------------------------------------------------
// CLOUDFLARE
// -------------------------------------------------------------------------

// cloudflareRecord mirrors the subset of the Cloudflare DNS record schema the
// updater reads. Other fields (priority, locked, etc.) are ignored.
type cloudflareRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

// cloudflareListResponse is the shape returned by GET /zones/.../dns_records.
type cloudflareListResponse struct {
	Success bool               `json:"success"`
	Result  []cloudflareRecord `json:"result"`
	Errors  []cloudflareError  `json:"errors"`
}

// cloudflareUpdateResponse is the shape returned by PATCH /dns_records/{id}.
type cloudflareUpdateResponse struct {
	Success bool              `json:"success"`
	Errors  []cloudflareError `json:"errors"`
}

// cloudflareError carries one entry from the Cloudflare API errors array.
type cloudflareError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// fetchRecord returns the current content and record ID of the configured
// hostname's A record. Caches the record ID for subsequent ticks.
func (u *Updater) fetchRecord(ctx context.Context) (string, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, cloudflareTimeout)
	defer cancel()

	endpoint := fmt.Sprintf("%s/zones/%s/dns_records?type=A&name=%s",
		u.baseURL, u.cfg.Cloudflare.ZoneID, u.cfg.Hostname,
	)

	body, err := u.cfDo(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}

	var parsed cloudflareListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("decode response: %w", err)
	}
	if !parsed.Success {
		return "", "", fmt.Errorf("cloudflare api: %s", joinErrors(parsed.Errors))
	}
	if len(parsed.Result) == 0 {
		return "", "", fmt.Errorf("no A record found for %s", u.cfg.Hostname)
	}

	rec := parsed.Result[0]
	u.recordIDCache = rec.ID
	return rec.Content, rec.ID, nil
}

// updateRecord PATCHes the named record to the supplied IPv4 content.
func (u *Updater) updateRecord(ctx context.Context, recordID, ip string) error {
	reqCtx, cancel := context.WithTimeout(ctx, cloudflareTimeout)
	defer cancel()

	endpoint := fmt.Sprintf("%s/zones/%s/dns_records/%s",
		u.baseURL, u.cfg.Cloudflare.ZoneID, recordID,
	)
	payload, err := json.Marshal(map[string]string{
		"type":    "A",
		"name":    u.cfg.Hostname,
		"content": ip,
	})
	if err != nil {
		return err
	}

	body, err := u.cfDo(reqCtx, http.MethodPatch, endpoint, payload)
	if err != nil {
		return err
	}

	var parsed cloudflareUpdateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !parsed.Success {
		return fmt.Errorf("cloudflare api: %s", joinErrors(parsed.Errors))
	}
	return nil
}

// cfDo issues an authenticated Cloudflare API request and returns the raw
// response body when the status code is 200. Adds the bearer token and
// Content-Type header.
func (u *Updater) cfDo(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// joinErrors flattens a Cloudflare errors array into a single human-readable
// message. Returns "<empty>" when no errors were supplied.
func joinErrors(errs []cloudflareError) string {
	if len(errs) == 0 {
		return "<empty>"
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = fmt.Sprintf("%d: %s", e.Code, e.Message)
	}
	return strings.Join(parts, "; ")
}

// -------------------------------------------------------------------------
// COOLDOWN AND METRICS HELPERS
// -------------------------------------------------------------------------

// inCooldown reports whether the updater is within the configured post-update
// cooldown window. The cooldown only applies after a successful update.
func (u *Updater) inCooldown() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lastUpdated.IsZero() {
		return false
	}
	return time.Since(u.lastUpdated) < u.cfg.Cooldown
}

// markUpdated records the time and IP of a successful update so subsequent
// ticks can honour the cooldown and rotate the gauge label cleanly.
func (u *Updater) markUpdated(ip string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastUpdated = time.Now()
	u.lastIP = ip
}

// recordCurrentIP updates the WanIPCurrent gauge so dashboards see the
// current WAN IP. Rotates the previous label value to avoid accumulating
// stale time series after every IP change.
func (u *Updater) recordCurrentIP(ip string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lastIP == ip {
		return
	}
	if u.lastIP != "" {
		metrics.WanIPCurrent.DeleteLabelValues(u.lastIP)
	}
	u.lastIP = ip
	metrics.WanIPCurrent.WithLabelValues(ip).Set(1)
}
