// -------------------------------------------------------------------------------
// Oracle Watchdog - Monitor Mode
//
// Project: Munchbox / Author: Alex Freidah
//
// Runs on each Oracle node and maintains a Consul session with TTL. The session
// acts as a heartbeat - if the node becomes unresponsive (due to Oracle
// reclamation), the session expires and the agent detects it.
//
// Self-healing design: never crashes due to Consul unavailability. Continuously
// attempts reconnection and emits metrics on current state.
// -------------------------------------------------------------------------------

package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	consul "github.com/hashicorp/consul/api"
	"go.opentelemetry.io/otel/codes"

	"github.com/afreidah/oracle-watchdog/internal/metrics"
	"github.com/afreidah/oracle-watchdog/internal/tracing"
)

const (
	sessionTTL     = "30s"
	renewInterval  = 10 * time.Second
	sessionKeyPath = "oracle-watchdog/nodes"
	metricsPort    = ":9104"

	// heartbeatInterval controls how often a healthy status log is emitted.
	// With a 10s renew interval, 30 renewals = every 5 minutes.
	heartbeatRenewals = 30
)

// -------------------------------------------------------------------------
// MONITOR STATE
// -------------------------------------------------------------------------

type state int

const (
	stateDisconnected state = iota
	stateConnecting
	stateActive
)

func (s state) String() string {
	switch s {
	case stateDisconnected:
		return "disconnected"
	case stateConnecting:
		return "connecting"
	case stateActive:
		return "active"
	default:
		return "unknown"
	}
}

// -------------------------------------------------------------------------
// MONITOR
// -------------------------------------------------------------------------

// Monitor maintains a Consul session heartbeat for an Oracle node.
type Monitor struct {
	nodeName      string
	consulAddress string

	mu            sync.RWMutex
	client        *consul.Client
	sessionID     string
	state         state
	renewCount    int
}

// New creates a Monitor for the given node name. Connection to Consul happens
// asynchronously in Run().
func New(nodeName string) *Monitor {
	consulAddr := "consul.service.consul:8500"
	if addr := os.Getenv("CONSUL_HTTP_ADDR"); addr != "" {
		consulAddr = addr
	}

	return &Monitor{
		nodeName:      nodeName,
		consulAddress: consulAddr,
		state:         stateDisconnected,
	}
}

// Run starts the monitor loop. Never returns an error due to Consul issues -
// continuously retries and emits metrics. Only returns on context cancellation.
func (m *Monitor) Run(ctx context.Context) error {
	metrics.RegisterMonitor()
	go metrics.Serve(ctx, metricsPort)

	ticker := time.NewTicker(renewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.cleanup(ctx)
			return nil
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

func (m *Monitor) tick(ctx context.Context) {
	m.mu.Lock()
	currentState := m.state
	m.mu.Unlock()

	switch currentState {
	case stateDisconnected:
		m.tryConnect(ctx)
	case stateConnecting:
		m.tryCreateSession(ctx)
	case stateActive:
		m.tryRenew(ctx)
	}
}

// -------------------------------------------------------------------------
// STATE TRANSITIONS
// -------------------------------------------------------------------------

func (m *Monitor) tryConnect(ctx context.Context) {
	metrics.MonitorReconnectAttempts.Inc()

	cfg := consul.DefaultConfig()
	cfg.Address = m.consulAddress

	client, err := consul.NewClient(cfg)
	if err != nil {
		slog.Warn("failed to create consul client", "error", err, "address", m.consulAddress)
		metrics.MonitorSessionFailures.Inc()
		return
	}

	// --- Verify connectivity with a simple operation ---
	_, span := tracing.StartClientSpan(ctx, "consul.status.leader",
		tracing.PeerServiceAttr("consul"),
		tracing.ServerAddressAttr(m.consulAddress),
	)

	_, err = client.Status().Leader()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		slog.Warn("consul not reachable", "error", err, "address", m.consulAddress)
		metrics.MonitorSessionFailures.Inc()
		return
	}

	span.SetStatus(codes.Ok, "leader found")
	span.End()

	m.mu.Lock()
	m.client = client
	m.state = stateConnecting
	m.mu.Unlock()

	metrics.MonitorConsulConnected.Set(1)
	slog.Info("connected to consul", "address", m.consulAddress)

	// --- Immediately try to create session ---
	m.tryCreateSession(ctx)
}

func (m *Monitor) tryCreateSession(ctx context.Context) {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()

	if client == nil {
		m.transitionTo(stateDisconnected)
		return
	}

	entry := &consul.SessionEntry{
		Name:      fmt.Sprintf("oracle-watchdog/%s", m.nodeName),
		TTL:       sessionTTL,
		Behavior:  consul.SessionBehaviorDelete,
		LockDelay: 0,
	}

	_, span := tracing.StartClientSpan(ctx, "consul.session.create",
		tracing.PeerServiceAttr("consul"),
		tracing.ServerAddressAttr(m.consulAddress),
		tracing.NodeAttr(m.nodeName),
	)

	sessionID, _, err := client.Session().Create(entry, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		slog.Warn("failed to create session", "error", err)
		metrics.MonitorSessionFailures.Inc()
		m.transitionTo(stateDisconnected)
		metrics.MonitorConsulConnected.Set(0)
		return
	}

	span.SetStatus(codes.Ok, "session created")
	span.End()

	// --- Write key associated with session ---
	if err := m.writeKey(ctx, client, sessionID); err != nil {
		slog.Warn("failed to write key", "error", err)
		metrics.MonitorSessionFailures.Inc()
		_, _ = client.Session().Destroy(sessionID, nil)
		m.transitionTo(stateDisconnected)
		metrics.MonitorConsulConnected.Set(0)
		return
	}

	m.mu.Lock()
	m.sessionID = sessionID
	m.state = stateActive
	m.renewCount = 0
	m.mu.Unlock()

	metrics.MonitorSessionActive.Set(1)
	slog.Info("session created", "session_id", sessionID, "node", m.nodeName)
}

func (m *Monitor) tryRenew(ctx context.Context) {
	m.mu.RLock()
	client := m.client
	sessionID := m.sessionID
	m.mu.RUnlock()

	if client == nil || sessionID == "" {
		m.transitionTo(stateDisconnected)
		return
	}

	_, span := tracing.StartClientSpan(ctx, "consul.session.renew",
		tracing.PeerServiceAttr("consul"),
		tracing.ServerAddressAttr(m.consulAddress),
	)

	entry, _, err := client.Session().Renew(sessionID, nil)
	if err != nil || entry == nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "renewal failed")
		span.End()
		slog.Warn("session renewal failed", "error", err, "session_id", sessionID)
		metrics.MonitorSessionFailures.Inc()
		metrics.MonitorSessionActive.Set(0)
		m.transitionTo(stateConnecting)
		return
	}

	span.SetStatus(codes.Ok, "renewed")
	span.End()

	metrics.MonitorSessionRenewals.Inc()
	m.renewCount++

	if m.renewCount%heartbeatRenewals == 0 {
		slog.Info("heartbeat", "node", m.nodeName, "session_id", sessionID, "renewals", m.renewCount)
	} else {
		slog.Debug("session renewed", "session_id", sessionID)
	}
}

func (m *Monitor) transitionTo(newState state) {
	m.mu.Lock()
	oldState := m.state
	m.state = newState

	if newState == stateDisconnected {
		m.client = nil
		m.sessionID = ""
		metrics.MonitorConsulConnected.Set(0)
		metrics.MonitorSessionActive.Set(0)
	}

	m.mu.Unlock()

	if oldState != newState {
		slog.Info("state transition", "from", oldState, "to", newState)
	}
}

func (m *Monitor) writeKey(ctx context.Context, client *consul.Client, sessionID string) error {
	_, span := tracing.StartClientSpan(ctx, "consul.kv.acquire",
		tracing.PeerServiceAttr("consul"),
		tracing.ServerAddressAttr(m.consulAddress),
		tracing.NodeAttr(m.nodeName),
	)
	defer span.End()

	kv := client.KV()
	key := fmt.Sprintf("%s/%s", sessionKeyPath, m.nodeName)

	pair := &consul.KVPair{
		Key:     key,
		Value:   []byte(time.Now().UTC().Format(time.RFC3339)),
		Session: sessionID,
	}

	acquired, _, err := kv.Acquire(pair, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if !acquired {
		err := fmt.Errorf("failed to acquire key lock")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	span.SetStatus(codes.Ok, "key written")
	slog.Debug("key written", "key", key)
	return nil
}

func (m *Monitor) cleanup(ctx context.Context) {
	m.mu.Lock()
	client := m.client
	sessionID := m.sessionID
	m.mu.Unlock()

	if client != nil && sessionID != "" {
		_, span := tracing.StartClientSpan(ctx, "consul.session.destroy",
			tracing.PeerServiceAttr("consul"),
			tracing.ServerAddressAttr(m.consulAddress),
		)

		slog.Info("destroying session", "session_id", sessionID)
		_, _ = client.Session().Destroy(sessionID, nil)

		span.SetStatus(codes.Ok, "session destroyed")
		span.End()
	}

	metrics.MonitorSessionActive.Set(0)
	metrics.MonitorConsulConnected.Set(0)
}
