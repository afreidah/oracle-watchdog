// -------------------------------------------------------------------------------
// Oracle Watchdog - Metrics
//
// Project: Munchbox / Author: Alex Freidah
//
// Shared Prometheus metrics infrastructure. Provides metric definitions for both
// monitor and agent modes, plus a reusable HTTP metrics server.
// -------------------------------------------------------------------------------

package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// monitorOnce ensures monitor metrics are registered with the default
	// Prometheus registry exactly once, even if RegisterMonitor is called
	// repeatedly across reloads or tests.
	monitorOnce sync.Once

	// agentOnce ensures agent metrics are registered exactly once.
	agentOnce sync.Once
)

// -------------------------------------------------------------------------
// MONITOR METRICS
// -------------------------------------------------------------------------

var (
	MonitorConsulConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_consul_connected",
		Help: "Whether the monitor is connected to Consul (1=connected, 0=disconnected)",
	})

	MonitorSessionActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_session_active",
		Help: "Whether the Consul session is active (1=active, 0=inactive)",
	})

	MonitorReconnectAttempts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oracle_watchdog_reconnect_attempts_total",
		Help: "Total number of Consul reconnection attempts",
	})

	MonitorSessionRenewals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oracle_watchdog_session_renewals_total",
		Help: "Total number of successful session renewals",
	})

	MonitorSessionFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oracle_watchdog_session_failures_total",
		Help: "Total number of session failures (creation or renewal)",
	})
)

// -------------------------------------------------------------------------
// MONITOR METRICS - WIREGUARD ENDPOINT RESOLVER
// -------------------------------------------------------------------------

var (
	// WgEndpointResolutionFailures counts every tick that failed before the
	// endpoint update could be attempted (DNS lookup error, peer not found,
	// netlink error). Operators should alert on a sustained non-zero rate.
	WgEndpointResolutionFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oracle_watchdog_wg_endpoint_resolution_failures_total",
		Help: "Total resolver ticks that failed before applying an endpoint update",
	})

	// WgEndpointChanges counts successful peer endpoint updates. A high rate
	// indicates the WAN IP is flapping or the stale-handshake threshold is
	// too aggressive.
	WgEndpointChanges = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oracle_watchdog_wg_endpoint_changes_total",
		Help: "Total successful peer endpoint updates applied via netlink",
	})

	// WgEndpointLastUpdate records the wall-clock time of the most recent
	// successful endpoint update. Useful for "time since last change"
	// dashboard panels.
	WgEndpointLastUpdate = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_wg_endpoint_last_update_timestamp_seconds",
		Help: "Unix timestamp of the most recent successful endpoint update",
	})

	// WgEndpointCurrentIP exposes the currently configured peer endpoint IP
	// as a label so dashboards can display it. The label is rotated (old
	// value deleted) on every change to avoid accumulating dead time series.
	WgEndpointCurrentIP = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "oracle_watchdog_wg_endpoint_current_ip",
		Help: "Always 1; the current peer endpoint IP is encoded in the ip label",
	}, []string{"interface", "peer", "ip"})

	// WgPeerHandshakeAge reports the seconds elapsed since the most recent
	// successful handshake with the tracked peer. A value of -1 indicates no
	// handshake has ever completed.
	WgPeerHandshakeAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "oracle_watchdog_wg_peer_handshake_age_seconds",
		Help: "Seconds since the most recent peer handshake; -1 if never",
	}, []string{"peer"})
)

// -------------------------------------------------------------------------
// AGENT METRICS
// -------------------------------------------------------------------------

var (
	AgentConsulConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_agent_consul_connected",
		Help: "Whether the agent is connected to Consul (1=connected, 0=disconnected)",
	})

	AgentOCIConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_agent_oci_connected",
		Help: "Whether the agent is connected to OCI (1=connected, 0=disconnected)",
	})

	AgentNodesMonitored = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_agent_nodes_monitored",
		Help: "Number of nodes being monitored",
	})

	AgentNodesMissing = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_agent_nodes_missing",
		Help: "Number of nodes currently missing",
	})

	AgentRestartAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_watchdog_agent_restart_attempts_total",
		Help: "Total restart attempts per node",
	}, []string{"node"})

	AgentRestartSuccesses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_watchdog_agent_restart_successes_total",
		Help: "Total successful restarts per node",
	}, []string{"node"})

	AgentRestartFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_watchdog_agent_restart_failures_total",
		Help: "Total failed restarts per node",
	}, []string{"node"})

	AgentConsulCheckFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oracle_watchdog_agent_consul_check_failures_total",
		Help: "Total Consul check failures",
	})
)

// -------------------------------------------------------------------------
// AGENT METRICS - WAN DNS UPDATER
// -------------------------------------------------------------------------

var (
	// WanIPCurrent exposes the most recently detected WAN IPv4 address as a
	// label. Always 1; the value lives in the ip label and is rotated on
	// change so only the active series remains.
	WanIPCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "oracle_watchdog_wan_ip_current",
		Help: "Always 1; the current detected WAN IPv4 is encoded in the ip label",
	}, []string{"ip"})

	// WanIPChanges counts how many times the detected WAN IP differed from
	// the previous reading. Drives the "WAN IP flap" alert.
	WanIPChanges = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "oracle_watchdog_wan_ip_changes_total",
		Help: "Total times the detected WAN IP changed between polls",
	})

	// CloudflareRecordUpdates counts attempted DNS record updates split by
	// outcome ("success" or "fail").
	CloudflareRecordUpdates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_watchdog_cloudflare_record_updates_total",
		Help: "Total Cloudflare DNS record update attempts",
	}, []string{"result"})

	// WanIPDetectionFailures counts WAN-IP detection failures per provider.
	// A provider that consistently fails should be replaced.
	WanIPDetectionFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "oracle_watchdog_wan_ip_detection_failures_total",
		Help: "Total WAN-IP detection failures per provider URL",
	}, []string{"provider"})

	// WanDNSLastCheck records the wall-clock time of the most recent
	// detection attempt regardless of outcome.
	WanDNSLastCheck = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_wan_dns_last_check_timestamp_seconds",
		Help: "Unix timestamp of the most recent WAN-IP detection attempt",
	})

	// WanDNSInCooldown reports whether the updater is currently within the
	// post-update cooldown window (1 = in cooldown, 0 = updates allowed).
	WanDNSInCooldown = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "oracle_watchdog_wan_dns_in_cooldown",
		Help: "Whether the WAN DNS updater is in the post-update cooldown window",
	})
)

// -------------------------------------------------------------------------
// REGISTRATION
// -------------------------------------------------------------------------

// RegisterMonitor registers all monitor-mode metrics including the WireGuard
// endpoint resolver metrics. Safe to call multiple times.
func RegisterMonitor() {
	monitorOnce.Do(func() {
		prometheus.MustRegister(
			MonitorConsulConnected,
			MonitorSessionActive,
			MonitorReconnectAttempts,
			MonitorSessionRenewals,
			MonitorSessionFailures,
			WgEndpointResolutionFailures,
			WgEndpointChanges,
			WgEndpointLastUpdate,
			WgEndpointCurrentIP,
			WgPeerHandshakeAge,
		)
	})
}

// RegisterAgent registers all agent-mode metrics including the WAN DNS
// updater metrics. Safe to call multiple times.
func RegisterAgent() {
	agentOnce.Do(func() {
		prometheus.MustRegister(
			AgentConsulConnected,
			AgentOCIConnected,
			AgentNodesMonitored,
			AgentNodesMissing,
			AgentRestartAttempts,
			AgentRestartSuccesses,
			AgentRestartFailures,
			AgentConsulCheckFailures,
			WanIPCurrent,
			WanIPChanges,
			CloudflareRecordUpdates,
			WanIPDetectionFailures,
			WanDNSLastCheck,
			WanDNSInCooldown,
		)
	})
}

// -------------------------------------------------------------------------
// SERVER
// -------------------------------------------------------------------------

// Serve starts a Prometheus metrics HTTP server on the given port. Blocks until
// context is cancelled. Port should include colon (e.g. ":9102").
func Serve(ctx context.Context, port string) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"status":"ok"}`)
	})

	server := &http.Server{
		Addr:              port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()

	slog.Info("metrics server starting", "port", port)
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		slog.Warn("metrics server error", "error", err)
	}
}
