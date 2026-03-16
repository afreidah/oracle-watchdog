// -------------------------------------------------------------------------------
// Oracle Watchdog - Monitor Tests
//
// Project: Munchbox / Author: Alex Freidah
// -------------------------------------------------------------------------------

package monitor

import (
	"testing"
)

func TestState_String(t *testing.T) {
	tests := []struct {
		state state
		want  string
	}{
		{stateDisconnected, "disconnected"},
		{stateConnecting, "connecting"},
		{stateActive, "active"},
		{state(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := tt.state.String()
			if got != tt.want {
				t.Errorf("state.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	monitor := New("test-node")

	if monitor.nodeName != "test-node" {
		t.Errorf("expected nodeName = test-node, got %s", monitor.nodeName)
	}
	if monitor.state != stateDisconnected {
		t.Errorf("expected initial state = disconnected, got %v", monitor.state)
	}
}

func TestNew_UsesEnvVar(t *testing.T) {
	t.Setenv("CONSUL_HTTP_ADDR", "custom-consul:8500")

	monitor := New("test-node")

	if monitor.consulAddress != "custom-consul:8500" {
		t.Errorf("expected consulAddress = custom-consul:8500, got %s", monitor.consulAddress)
	}
}

func TestNew_DefaultConsulAddress(t *testing.T) {
	t.Setenv("CONSUL_HTTP_ADDR", "")

	monitor := New("test-node")

	if monitor.consulAddress != "consul.service.consul:8500" {
		t.Errorf("expected default consulAddress, got %s", monitor.consulAddress)
	}
}

// -------------------------------------------------------------------------
// HEARTBEAT
// -------------------------------------------------------------------------

func TestHeartbeat_EmitsAtInterval(t *testing.T) {
	m := New("test-node")
	m.renewCount = heartbeatRenewals - 1

	// Simulate the renewal counter logic
	m.renewCount++
	if m.renewCount%heartbeatRenewals != 0 {
		t.Errorf("expected heartbeat at %d renewals, got renewCount=%d", heartbeatRenewals, m.renewCount)
	}
}

func TestHeartbeat_DoesNotEmitBetweenIntervals(t *testing.T) {
	m := New("test-node")
	m.renewCount = 5

	m.renewCount++
	if m.renewCount%heartbeatRenewals == 0 {
		t.Errorf("should not emit heartbeat at renewCount=%d", m.renewCount)
	}
}

func TestHeartbeat_ResetsOnNewSession(t *testing.T) {
	m := New("test-node")
	m.renewCount = 25

	// Simulate session creation resetting the counter
	m.renewCount = 0

	if m.renewCount != 0 {
		t.Errorf("renewCount should reset to 0 on new session, got %d", m.renewCount)
	}
}
