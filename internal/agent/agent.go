// -------------------------------------------------------------------------------
// Oracle Watchdog - Agent Mode
//
// Author: Alex Freidah
//
// Runs on homelab infrastructure and monitors Consul for missing Oracle node
// sessions. When a node's session has been absent longer than the configured
// timeout, triggers an OCI stop/start cycle to recover the instance.
//
// Self-healing design: never crashes due to Consul or OCI unavailability.
// Continuously attempts reconnection and emits metrics on current state.
// -------------------------------------------------------------------------------

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/afreidah/oracle-watchdog/internal/config"
	"github.com/afreidah/oracle-watchdog/internal/metrics"
	"github.com/afreidah/oracle-watchdog/internal/tracing"
	"github.com/afreidah/oracle-watchdog/internal/wandns"

	consul "github.com/hashicorp/consul/api"
	"go.opentelemetry.io/otel/codes"
)

const (
	sessionKeyPath         = "oracle-watchdog/nodes"
	metricsPort            = ":9105"
	maxConsecutiveFailures = 3
)

// -------------------------------------------------------------------------
// AGENT STATE
// -------------------------------------------------------------------------

type connectionState int

const (
	stateDisconnected connectionState = iota
	stateConnected
)

func (s connectionState) String() string {
	switch s {
	case stateDisconnected:
		return "disconnected"
	case stateConnected:
		return "connected"
	default:
		return "unknown"
	}
}

// -------------------------------------------------------------------------
// AGENT
// -------------------------------------------------------------------------

// Agent monitors Oracle nodes and restarts unresponsive instances.
type Agent struct {
	cfg *config.Config

	mu          sync.RWMutex
	consul      ConsulClient
	oci         InstanceRestarter
	consulState connectionState
	ociState    connectionState

	// Client factories, injectable for tests. Default to the real adapters
	// (newConsulClient / newOCIClient) wired in New().
	newConsul func(address string) (ConsulClient, error)
	newOCI    func(configPath, profile string) (InstanceRestarter, error)

	// Tracks when each node was first seen as missing
	missingSince map[string]time.Time

	// Tracks nodes currently being restarted (prevent duplicate restarts)
	restarting map[string]bool

	// Tracks consecutive restart attempts per node (resets on recovery)
	restartAttempts map[string]int

	// Waits for in-flight restart goroutines during shutdown
	restartWg sync.WaitGroup

	// Consecutive failure tracking for connection health
	consulFailures int
	ociFailures    int
}

// Option configures the Agent. Used to inject dependencies (chiefly the Consul
// and OCI client factories) without changing New's signature for existing
// callers - primarily for integration tests that pair a real Consul with a
// substituted OCI client.
type Option func(*Agent)

// WithConsulClientFactory overrides the Consul client factory.
func WithConsulClientFactory(f func(address string) (ConsulClient, error)) Option {
	return func(a *Agent) { a.newConsul = f }
}

// WithOCIClientFactory overrides the OCI client factory, letting tests inject a
// fake InstanceRestarter in place of the real OCI SDK client.
func WithOCIClientFactory(f func(configPath, profile string) (InstanceRestarter, error)) Option {
	return func(a *Agent) { a.newOCI = f }
}

// New creates an Agent with the given configuration. Connections to Consul
// and OCI happen asynchronously in Run().
func New(cfg *config.Config, opts ...Option) *Agent {
	metrics.AgentNodesMonitored.Set(float64(len(cfg.Nodes)))

	// Initialize restart counters so metrics exist in Prometheus even with zero restarts.
	for _, node := range cfg.Nodes {
		metrics.AgentRestartAttempts.WithLabelValues(node.Name)
		metrics.AgentRestartSuccesses.WithLabelValues(node.Name)
		metrics.AgentRestartFailures.WithLabelValues(node.Name)
	}

	a := &Agent{
		cfg:             cfg,
		consulState:     stateDisconnected,
		ociState:        stateDisconnected,
		missingSince:    make(map[string]time.Time),
		restarting:      make(map[string]bool),
		restartAttempts: make(map[string]int),
		newConsul:       newConsulClient,
		newOCI:          newOCIClient,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run starts the monitoring loop. Never returns an error due to connection
// issues - continuously retries and emits metrics. Only returns on context
// cancellation. When wan_dns is enabled in config, the Updater runs as a
// sibling goroutine sharing the same context lifetime.
func (a *Agent) Run(ctx context.Context) error {
	metrics.RegisterAgent()
	go metrics.Serve(ctx, metricsPort)

	a.startWanDNSUpdater(ctx)

	ticker := time.NewTicker(a.cfg.CheckInterval)
	defer ticker.Stop()

	// --- Initial connection attempts ---
	a.tryConnectConsul()
	a.tryConnectOCI()

	for {
		select {
		case <-ctx.Done():
			a.restartWg.Wait()
			return nil
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

// startWanDNSUpdater constructs and runs the WAN DNS updater in a background
// goroutine when wan_dns is enabled. Construction failures are logged and the
// agent continues without the updater - DDNS is an optional feature and must
// not block the missing-session detection loop.
func (a *Agent) startWanDNSUpdater(ctx context.Context) {
	if !a.cfg.WanDNS.Enabled {
		return
	}
	u, err := wandns.New(a.cfg.WanDNS)
	if err != nil {
		slog.Warn("failed to start wan dns updater", "error", err)
		return
	}
	go u.Run(ctx)
}

func (a *Agent) tick(ctx context.Context) {
	a.mu.RLock()
	consulState := a.consulState
	ociState := a.ociState
	a.mu.RUnlock()

	// --- Ensure connections ---
	if consulState == stateDisconnected {
		a.tryConnectConsul()
	}
	if ociState == stateDisconnected {
		a.tryConnectOCI()
	}

	// --- Check nodes if Consul is connected ---
	a.mu.RLock()
	consulState = a.consulState
	a.mu.RUnlock()

	if consulState == stateConnected {
		a.checkNodes(ctx)
	}
}

// -------------------------------------------------------------------------
// CONNECTION MANAGEMENT
// -------------------------------------------------------------------------

func (a *Agent) tryConnectConsul() {
	client, err := a.newConsul(a.cfg.ConsulAddress)
	if err != nil {
		slog.Warn("failed to create consul client", "error", err)
		return
	}

	// --- Verify connectivity ---
	if _, err := client.Leader(); err != nil {
		slog.Warn("consul not reachable", "error", err, "address", a.cfg.ConsulAddress)
		return
	}

	a.mu.Lock()
	a.consul = client
	a.consulState = stateConnected
	a.consulFailures = 0
	a.mu.Unlock()

	metrics.AgentConsulConnected.Set(1)
	slog.Info("connected to consul", "address", a.cfg.ConsulAddress)
}

func (a *Agent) tryConnectOCI() {
	client, err := a.newOCI(a.cfg.OCI.ConfigPath, a.cfg.OCI.Profile)
	if err != nil {
		slog.Warn("failed to create oci client", "error", err)
		return
	}

	a.mu.Lock()
	a.oci = client
	a.ociState = stateConnected
	a.ociFailures = 0
	a.mu.Unlock()

	metrics.AgentOCIConnected.Set(1)
	slog.Info("connected to oci", "profile", a.cfg.OCI.Profile)
}

func (a *Agent) markConsulDisconnected() {
	a.mu.Lock()
	a.consul = nil
	a.consulState = stateDisconnected
	a.mu.Unlock()

	metrics.AgentConsulConnected.Set(0)
	slog.Warn("consul connection lost")
}

func (a *Agent) markOCIDisconnected() {
	a.mu.Lock()
	a.oci = nil
	a.ociState = stateDisconnected
	a.mu.Unlock()

	metrics.AgentOCIConnected.Set(0)
	slog.Warn("oci connection lost")
}

// -------------------------------------------------------------------------
// NODE MONITORING
// -------------------------------------------------------------------------

func (a *Agent) checkNodes(ctx context.Context) {
	ctx, span := tracing.StartSpan(ctx, "agent.check_nodes")
	defer span.End()

	a.mu.RLock()
	client := a.consul
	a.mu.RUnlock()

	if client == nil {
		span.SetStatus(codes.Error, "consul client nil")
		return
	}

	now := time.Now()
	missingCount := 0
	hadError := false

	for _, node := range a.cfg.Nodes {
		pair, err := a.getNodeKV(ctx, client, node)
		if err != nil {
			hadError = true
			span.RecordError(err)

			// --- Obvious connection errors abort the pass immediately ---
			if isConnectionError(err) {
				a.markConsulDisconnected()
				span.SetStatus(codes.Error, "consul connection lost")
				return
			}
			continue
		}

		if pair == nil {
			a.handleMissingNode(ctx, node, now)
			missingCount++
		} else {
			a.handleAliveNode(node)
		}
	}

	metrics.AgentNodesMissing.Set(float64(missingCount))

	if a.trackConsulFailures(hadError) {
		span.SetStatus(codes.Error, "consecutive failures exceeded")
		return
	}

	span.SetAttributes(
		tracing.DurationAttr("nodes_missing", float64(missingCount)),
		tracing.DurationAttr("nodes_total", float64(len(a.cfg.Nodes))),
	)
	span.SetStatus(codes.Ok, "check complete")
}

// getNodeKV reads a single node's session key from Consul, wrapping the call in
// a client span and incrementing the check-failure metric on error.
func (a *Agent) getNodeKV(ctx context.Context, client ConsulClient, node config.NodeConfig) (*consul.KVPair, error) {
	key := fmt.Sprintf("%s/%s", sessionKeyPath, node.Name)

	_, kvSpan := tracing.StartClientSpan(ctx, "consul.kv.get",
		tracing.PeerServiceAttr("consul"),
		tracing.ServerAddressAttr(a.cfg.ConsulAddress),
		tracing.NodeAttr(node.Name),
	)
	defer kvSpan.End()

	pair, err := client.GetKV(key)
	if err != nil {
		kvSpan.RecordError(err)
		kvSpan.SetStatus(codes.Error, err.Error())
		slog.Warn("failed to check node key", "node", node.Name, "error", err)
		metrics.AgentConsulCheckFailures.Inc()
		return nil, err
	}

	kvSpan.SetStatus(codes.Ok, "key read")
	return pair, nil
}

// handleMissingNode processes a node whose session key is absent. It records the
// first-missing time, and once the configured timeout has elapsed (and restart
// limits permit) launches an async restart. All state is mutated under the
// agent mutex.
func (a *Agent) handleMissingNode(ctx context.Context, node config.NodeConfig, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Already restarting, or already at the max-attempts ceiling: nothing to do.
	if _, isRestarting := a.restarting[node.Name]; isRestarting {
		return
	}
	if a.cfg.MaxRestartAttempts > 0 && a.restartAttempts[node.Name] >= a.cfg.MaxRestartAttempts {
		return
	}

	firstMissing, exists := a.missingSince[node.Name]
	if !exists {
		slog.Info("node went missing", "node", node.Name)
		a.missingSince[node.Name] = now
		return
	}

	missingDuration := now.Sub(firstMissing)
	if missingDuration < a.cfg.Timeout {
		slog.Info("node missing",
			"node", node.Name,
			"missing_for", missingDuration,
			"timeout_in", a.cfg.Timeout-missingDuration,
		)
		return
	}

	slog.Warn("node exceeded timeout, triggering restart",
		"node", node.Name,
		"missing_for", missingDuration,
		"timeout", a.cfg.Timeout,
		"attempt", a.restartAttempts[node.Name]+1,
	)

	// Mark as restarting and clear tracking, then restart in a goroutine so the
	// remaining node checks are not blocked.
	a.restarting[node.Name] = true
	delete(a.missingSince, node.Name)
	a.restartWg.Add(1)
	go a.restartNode(ctx, node)
}

// handleAliveNode processes a node whose session key is present, clearing any
// missing/restart tracking so a recovered node starts from a clean slate.
func (a *Agent) handleAliveNode(node config.NodeConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, wasMissing := a.missingSince[node.Name]; wasMissing {
		slog.Info("node recovered", "node", node.Name)
		delete(a.missingSince, node.Name)
	}
	// Reset restart attempts on recovery.
	if a.restartAttempts[node.Name] > 0 {
		slog.Info("resetting restart attempts", "node", node.Name, "previous_attempts", a.restartAttempts[node.Name])
		delete(a.restartAttempts, node.Name)
	}
}

// trackConsulFailures updates the consecutive-failure counter after a check
// pass. It returns true when the failure threshold has been crossed, in which
// case Consul has been marked disconnected and the caller should stop.
func (a *Agent) trackConsulFailures(hadError bool) bool {
	a.mu.Lock()

	if !hadError {
		a.consulFailures = 0
		a.mu.Unlock()
		return false
	}

	a.consulFailures++
	if a.consulFailures < maxConsecutiveFailures {
		a.mu.Unlock()
		return false
	}

	failures := a.consulFailures
	a.mu.Unlock()

	slog.Warn("consul consecutive failures exceeded threshold",
		"failures", failures,
		"threshold", maxConsecutiveFailures,
	)
	a.markConsulDisconnected()
	return true
}

func (a *Agent) restartNode(ctx context.Context, node config.NodeConfig) {
	defer a.restartWg.Done()

	ctx, span := tracing.StartSpan(ctx, "agent.restart_node",
		tracing.NodeAttr(node.Name),
		tracing.InstanceAttr(node.InstanceID),
	)
	defer span.End()

	defer func() {
		a.mu.Lock()
		delete(a.restarting, node.Name)
		a.mu.Unlock()
	}()

	a.mu.RLock()
	ociClient := a.oci
	ociState := a.ociState
	a.mu.RUnlock()

	if ociState == stateDisconnected || ociClient == nil {
		slog.Warn("cannot restart node, oci disconnected", "node", node.Name)
		metrics.AgentRestartFailures.WithLabelValues(node.Name).Inc()
		span.SetStatus(codes.Error, "oci disconnected")
		return
	}

	metrics.AgentRestartAttempts.WithLabelValues(node.Name).Inc()

	// --- Dry-run mode: log but don't execute ---
	if a.cfg.DryRun {
		slog.Info("[DRY-RUN] would initiate OCI restart",
			"node", node.Name,
			"instance_id", node.InstanceID,
			"compartment_id", node.CompartmentID,
		)
		metrics.AgentRestartSuccesses.WithLabelValues(node.Name).Inc()
		span.SetStatus(codes.Ok, "dry-run restart simulated")
		return
	}

	// Increment restart attempts counter
	a.mu.Lock()
	a.restartAttempts[node.Name]++
	attempt := a.restartAttempts[node.Name]
	a.mu.Unlock()

	slog.Info("initiating OCI restart", "node", node.Name, "instance_id", node.InstanceID, "attempt", attempt)

	if err := ociClient.RestartInstance(ctx, node.InstanceID, node.CompartmentID); err != nil {
		slog.Error("restart failed", "node", node.Name, "error", err)
		metrics.AgentRestartFailures.WithLabelValues(node.Name).Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		// --- Track OCI failures ---
		a.mu.Lock()
		a.ociFailures++
		shouldDisconnect := a.ociFailures >= maxConsecutiveFailures || isConnectionError(err)
		a.mu.Unlock()

		if shouldDisconnect {
			slog.Warn("oci failures exceeded threshold or connection error detected",
				"failures", a.ociFailures,
			)
			a.markOCIDisconnected()
		}
		return
	}

	// --- Success - reset failure counter ---
	a.mu.Lock()
	a.ociFailures = 0
	a.mu.Unlock()

	metrics.AgentRestartSuccesses.WithLabelValues(node.Name).Inc()
	slog.Info("restart completed", "node", node.Name)
	span.SetStatus(codes.Ok, "restart completed")
}

// -------------------------------------------------------------------------
// UTILITIES
// -------------------------------------------------------------------------

// connectionErrorPatterns are substrings that indicate a connection-level failure.
var connectionErrorPatterns = []string{
	"connection refused",
	"connection reset",
	"no such host",
	"timeout",
	"deadline exceeded",
	"eof",
	"network unreachable",
	"no route to host",
	"broken pipe",
	"connection timed out",
	"i/o timeout",
	"dial tcp",
	"dial udp",
}

func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	for _, pattern := range connectionErrorPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}
