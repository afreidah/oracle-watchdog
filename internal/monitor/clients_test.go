// -------------------------------------------------------------------------------
// Oracle Watchdog - Monitor Session Lifecycle Tests
//
// Author: Alex Freidah
//
// Exercises the connect -> create-session -> renew -> destroy state machine
// through the ConsulSession consumer interface, using an in-memory fake in
// place of the real Consul SDK client.
// -------------------------------------------------------------------------------

package monitor

import (
	"context"
	"errors"
	"testing"

	"github.com/afreidah/oracle-watchdog/internal/config"

	consul "github.com/hashicorp/consul/api"
)

// --- Fake -----------------------------------------------------------------

// fakeConsul is an in-memory ConsulSession with configurable outcomes for each
// operation and recorders for the calls the monitor makes.
type fakeConsul struct {
	leaderErr error

	createID  string
	createErr error
	created   []*consul.SessionEntry

	renewEntry *consul.SessionEntry
	renewErr   error

	destroyErr error
	destroyed  []string

	acquireOK  bool
	acquireErr error
	acquired   []*consul.KVPair
}

func (f *fakeConsul) Leader() (string, error) { return "leader:8300", f.leaderErr }

func (f *fakeConsul) CreateSession(entry *consul.SessionEntry) (string, error) {
	f.created = append(f.created, entry)
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.createID, nil
}

func (f *fakeConsul) RenewSession(sessionID string) (*consul.SessionEntry, error) {
	return f.renewEntry, f.renewErr
}

func (f *fakeConsul) DestroySession(sessionID string) error {
	f.destroyed = append(f.destroyed, sessionID)
	return f.destroyErr
}

func (f *fakeConsul) AcquireKey(pair *consul.KVPair) (bool, error) {
	f.acquired = append(f.acquired, pair)
	if f.acquireErr != nil {
		return false, f.acquireErr
	}
	return f.acquireOK, nil
}

// healthyConsul returns a fake whose create/acquire/renew all succeed.
func healthyConsul() *fakeConsul {
	return &fakeConsul{
		createID:   "sess-123",
		acquireOK:  true,
		renewEntry: &consul.SessionEntry{ID: "sess-123"},
	}
}

func testMonitor() *Monitor { return New("test-node") }

// --- tryConnect -----------------------------------------------------------

func TestTryConnect_SuccessReachesActive(t *testing.T) {
	m := testMonitor()
	fc := healthyConsul()
	m.newConsul = func(string) (ConsulSession, error) { return fc, nil }

	m.tryConnect(context.Background())

	// tryConnect immediately drives tryCreateSession on success.
	if m.state != stateActive {
		t.Fatalf("expected active after successful connect, got %v", m.state)
	}
	if m.sessionID != "sess-123" {
		t.Errorf("expected sessionID set, got %q", m.sessionID)
	}
	if len(fc.created) != 1 || len(fc.acquired) != 1 {
		t.Errorf("expected one session create and one key acquire, got %d/%d", len(fc.created), len(fc.acquired))
	}
}

func TestTryConnect_FactoryErrorStaysDisconnected(t *testing.T) {
	m := testMonitor()
	m.newConsul = func(string) (ConsulSession, error) { return nil, errors.New("boom") }

	m.tryConnect(context.Background())

	if m.state != stateDisconnected {
		t.Errorf("expected disconnected on factory error, got %v", m.state)
	}
}

func TestTryConnect_LeaderErrorStaysDisconnected(t *testing.T) {
	m := testMonitor()
	m.newConsul = func(string) (ConsulSession, error) {
		return &fakeConsul{leaderErr: errors.New("no leader")}, nil
	}

	m.tryConnect(context.Background())

	if m.state != stateDisconnected {
		t.Errorf("expected disconnected when leader unreachable, got %v", m.state)
	}
}

// --- tryCreateSession -----------------------------------------------------

func TestTryCreateSession_NilClientDisconnects(t *testing.T) {
	m := testMonitor()
	m.client = nil
	m.state = stateConnecting

	m.tryCreateSession(context.Background())

	if m.state != stateDisconnected {
		t.Errorf("expected disconnected with nil client, got %v", m.state)
	}
}

func TestTryCreateSession_CreateErrorDisconnects(t *testing.T) {
	m := testMonitor()
	m.client = &fakeConsul{createErr: errors.New("create failed")}
	m.state = stateConnecting

	m.tryCreateSession(context.Background())

	if m.state != stateDisconnected {
		t.Errorf("expected disconnected on create error, got %v", m.state)
	}
}

func TestTryCreateSession_AcquireFailureDestroysAndDisconnects(t *testing.T) {
	m := testMonitor()
	fc := &fakeConsul{createID: "sess-x", acquireOK: false} // acquire returns false
	m.client = fc
	m.state = stateConnecting

	m.tryCreateSession(context.Background())

	if m.state != stateDisconnected {
		t.Errorf("expected disconnected when key acquire fails, got %v", m.state)
	}
	if len(fc.destroyed) != 1 || fc.destroyed[0] != "sess-x" {
		t.Errorf("expected the orphaned session to be destroyed, got %v", fc.destroyed)
	}
}

func TestTryCreateSession_SuccessActivates(t *testing.T) {
	m := testMonitor()
	m.client = healthyConsul()
	m.state = stateConnecting

	m.tryCreateSession(context.Background())

	if m.state != stateActive {
		t.Fatalf("expected active, got %v", m.state)
	}
	if m.sessionID != "sess-123" {
		t.Errorf("expected sessionID set, got %q", m.sessionID)
	}
	if m.renewCount != 0 {
		t.Errorf("expected renewCount reset to 0, got %d", m.renewCount)
	}
}

// --- tryRenew -------------------------------------------------------------

func TestTryRenew_SuccessIncrementsCount(t *testing.T) {
	m := testMonitor()
	m.client = healthyConsul()
	m.sessionID = "sess-123"
	m.state = stateActive

	m.tryRenew(context.Background())

	if m.state != stateActive {
		t.Errorf("expected to stay active after renew, got %v", m.state)
	}
	if m.renewCount != 1 {
		t.Errorf("expected renewCount=1, got %d", m.renewCount)
	}
}

func TestTryRenew_NilClientDisconnects(t *testing.T) {
	m := testMonitor()
	m.client = nil
	m.sessionID = ""
	m.state = stateActive

	m.tryRenew(context.Background())

	if m.state != stateDisconnected {
		t.Errorf("expected disconnected with nil client, got %v", m.state)
	}
}

func TestTryRenew_ErrorFallsBackToConnecting(t *testing.T) {
	m := testMonitor()
	m.client = &fakeConsul{renewErr: errors.New("renew failed")}
	m.sessionID = "sess-123"
	m.state = stateActive

	m.tryRenew(context.Background())

	if m.state != stateConnecting {
		t.Errorf("expected connecting after renew error, got %v", m.state)
	}
}

func TestTryRenew_NilEntryFallsBackToConnecting(t *testing.T) {
	m := testMonitor()
	m.client = &fakeConsul{renewEntry: nil} // gone session: nil entry, nil error
	m.sessionID = "sess-123"
	m.state = stateActive

	m.tryRenew(context.Background())

	if m.state != stateConnecting {
		t.Errorf("expected connecting when session is gone, got %v", m.state)
	}
}

func TestTryRenew_HeartbeatBoundary(t *testing.T) {
	m := testMonitor()
	m.client = healthyConsul()
	m.sessionID = "sess-123"
	m.state = stateActive
	m.renewCount = heartbeatRenewals - 1 // next renew hits the heartbeat log path

	m.tryRenew(context.Background())

	if m.renewCount != heartbeatRenewals {
		t.Errorf("expected renewCount=%d, got %d", heartbeatRenewals, m.renewCount)
	}
}

// --- transitions, tick, cleanup -------------------------------------------

func TestTransitionTo_DisconnectedClearsConnection(t *testing.T) {
	m := testMonitor()
	m.client = healthyConsul()
	m.sessionID = "sess-123"
	m.state = stateActive

	m.transitionTo(stateDisconnected)

	if m.client != nil || m.sessionID != "" {
		t.Errorf("expected client/sessionID cleared, got client=%v sessionID=%q", m.client, m.sessionID)
	}
}

func TestTick_DispatchesByState(t *testing.T) {
	// disconnected -> tryConnect (uses factory)
	t.Run("disconnected connects", func(t *testing.T) {
		m := testMonitor()
		m.newConsul = func(string) (ConsulSession, error) { return healthyConsul(), nil }
		m.state = stateDisconnected

		m.tick(context.Background())

		if m.state != stateActive {
			t.Errorf("expected active after tick from disconnected, got %v", m.state)
		}
	})

	// active -> tryRenew
	t.Run("active renews", func(t *testing.T) {
		m := testMonitor()
		m.client = healthyConsul()
		m.sessionID = "sess-123"
		m.state = stateActive

		m.tick(context.Background())

		if m.renewCount != 1 {
			t.Errorf("expected a renewal on tick from active, got renewCount=%d", m.renewCount)
		}
	})
}

func TestCleanup_DestroysActiveSession(t *testing.T) {
	m := testMonitor()
	fc := healthyConsul()
	m.client = fc
	m.sessionID = "sess-123"
	m.state = stateActive

	m.cleanup(context.Background())

	if len(fc.destroyed) != 1 || fc.destroyed[0] != "sess-123" {
		t.Errorf("expected session destroyed on cleanup, got %v", fc.destroyed)
	}
}

func TestCleanup_NoSessionIsNoop(t *testing.T) {
	m := testMonitor()
	// No client/session: cleanup must not panic.
	m.cleanup(context.Background())
}

func TestTick_ConnectingCreatesSession(t *testing.T) {
	m := testMonitor()
	m.client = healthyConsul()
	m.state = stateConnecting

	m.tick(context.Background())

	if m.state != stateActive {
		t.Errorf("expected active after tick from connecting, got %v", m.state)
	}
}

func TestWithWireguard_SetsConfig(t *testing.T) {
	cfg := config.WireguardConfig{Enabled: true, Interface: "wg0"}
	m := New("test-node", WithWireguard(cfg))

	if !m.wgCfg.Enabled || m.wgCfg.Interface != "wg0" {
		t.Errorf("WithWireguard did not apply config, got %+v", m.wgCfg)
	}
}

func TestStartWireguardResolver_DisabledIsNoop(t *testing.T) {
	m := testMonitor()
	m.wgCfg = config.WireguardConfig{Enabled: false}

	// Disabled: must return immediately without starting a goroutine or panicking.
	m.startWireguardResolver(context.Background())
}

func TestWriteKey_AcquireErrorPropagates(t *testing.T) {
	m := testMonitor()
	fc := &fakeConsul{acquireErr: errors.New("acquire failed")}

	if err := m.writeKey(context.Background(), fc, "sess-123"); err == nil {
		t.Error("expected writeKey to return the acquire error")
	}
}
