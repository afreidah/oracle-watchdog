// -------------------------------------------------------------------------------
// Oracle Watchdog - Agent Core Logic Tests
//
// Author: Alex Freidah
//
// Exercises the node-monitoring and connection logic through the ConsulClient
// and InstanceRestarter consumer interfaces, using in-memory fakes in place of the real
// Consul and OCI SDK clients.
// -------------------------------------------------------------------------------

package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/afreidah/oracle-watchdog/internal/config"

	consul "github.com/hashicorp/consul/api"
)

// --- Fakes ----------------------------------------------------------------

// fakeConsul is an in-memory ConsulClient. pairs is keyed by the full KV path;
// a missing entry yields a nil pair (an absent session key).
type fakeConsul struct {
	leaderErr error
	pairs     map[string]*consul.KVPair
	getErr    error
	getCalls  int
}

func (f *fakeConsul) Leader() (string, error) { return "leader:8300", f.leaderErr }

func (f *fakeConsul) GetKV(key string) (*consul.KVPair, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.pairs[key], nil
}

// fakeOCI is an in-memory InstanceRestarter recording the instances it was asked to
// restart.
type fakeOCI struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeOCI) RestartInstance(_ context.Context, instanceID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, instanceID)
	return f.err
}

func (f *fakeOCI) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// --- Helpers --------------------------------------------------------------

func nodeKey(name string) string {
	return fmt.Sprintf("%s/%s", sessionKeyPath, name)
}

func testAgent(node config.NodeConfig) *Agent {
	cfg := &config.Config{
		Timeout:       5 * time.Minute,
		CheckInterval: 30 * time.Second,
		ConsulAddress: "localhost:8500",
		Nodes:         []config.NodeConfig{node},
	}
	return New(cfg)
}

// --- checkNodes -----------------------------------------------------------

func TestCheckNodes_AliveNodeClearsTracking(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.consul = &fakeConsul{pairs: map[string]*consul.KVPair{nodeKey("n1"): {Key: nodeKey("n1")}}}
	a.consulState = stateConnected

	// Pretend the node was previously missing with prior restart attempts.
	a.missingSince["n1"] = time.Now().Add(-time.Hour)
	a.restartAttempts["n1"] = 2

	a.checkNodes(context.Background())

	if _, ok := a.missingSince["n1"]; ok {
		t.Error("expected missingSince cleared for recovered node")
	}
	if _, ok := a.restartAttempts["n1"]; ok {
		t.Error("expected restartAttempts cleared for recovered node")
	}
}

func TestCheckNodes_MissingFirstTimeRecordsTimestamp(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.consul = &fakeConsul{} // no pair -> missing
	a.consulState = stateConnected
	oci := &fakeOCI{}
	a.oci = oci
	a.ociState = stateConnected

	a.checkNodes(context.Background())
	a.restartWg.Wait()

	if _, ok := a.missingSince["n1"]; !ok {
		t.Error("expected missingSince recorded on first missing observation")
	}
	if oci.restartCount() != 0 {
		t.Errorf("expected no restart on first missing observation, got %d", oci.restartCount())
	}
}

func TestCheckNodes_MissingPastTimeoutTriggersRestart(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.cfg.Timeout = time.Millisecond
	a.consul = &fakeConsul{} // missing
	a.consulState = stateConnected
	oci := &fakeOCI{}
	a.oci = oci
	a.ociState = stateConnected

	// Already seen missing long enough ago to exceed the timeout.
	a.missingSince["n1"] = time.Now().Add(-time.Hour)

	a.checkNodes(context.Background())
	a.restartWg.Wait()

	if oci.restartCount() != 1 {
		t.Fatalf("expected 1 restart, got %d", oci.restartCount())
	}
	if oci.calls[0] != "i-1" {
		t.Errorf("restarted wrong instance: %q", oci.calls[0])
	}
	if a.restartAttempts["n1"] != 1 {
		t.Errorf("expected restartAttempts=1, got %d", a.restartAttempts["n1"])
	}
}

func TestCheckNodes_MaxAttemptsBlocksRestart(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.cfg.Timeout = time.Millisecond
	a.cfg.MaxRestartAttempts = 1
	a.consul = &fakeConsul{} // missing
	a.consulState = stateConnected
	oci := &fakeOCI{}
	a.oci = oci
	a.ociState = stateConnected

	a.missingSince["n1"] = time.Now().Add(-time.Hour)
	a.restartAttempts["n1"] = 1 // already at the ceiling

	a.checkNodes(context.Background())
	a.restartWg.Wait()

	if oci.restartCount() != 0 {
		t.Errorf("expected no restart at max attempts, got %d", oci.restartCount())
	}
}

func TestCheckNodes_NilClientReturnsEarly(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.consul = nil // connected state but no client (defensive guard)
	a.consulState = stateConnected

	// Must not panic and must leave tracking untouched.
	a.checkNodes(context.Background())

	if len(a.missingSince) != 0 {
		t.Error("expected no tracking changes when client is nil")
	}
}

func TestCheckNodes_AlreadyRestartingSkips(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.cfg.Timeout = time.Millisecond
	a.consul = &fakeConsul{} // missing
	a.consulState = stateConnected
	oci := &fakeOCI{}
	a.oci = oci
	a.ociState = stateConnected

	// A restart is already in flight for this node.
	a.restarting["n1"] = true
	a.missingSince["n1"] = time.Now().Add(-time.Hour)

	a.checkNodes(context.Background())
	a.restartWg.Wait()

	if oci.restartCount() != 0 {
		t.Errorf("expected no new restart while one is in flight, got %d", oci.restartCount())
	}
}

func TestCheckNodes_ConnectionErrorDisconnects(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.consul = &fakeConsul{getErr: errors.New("dial tcp: connection refused")}
	a.consulState = stateConnected

	a.checkNodes(context.Background())

	if a.consulState != stateDisconnected {
		t.Errorf("expected disconnected after connection error, got %v", a.consulState)
	}
}

func TestCheckNodes_NonConnectionErrorTripsThreshold(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.consul = &fakeConsul{getErr: errors.New("internal server error")}
	a.consulState = stateConnected

	// A non-connection error does not disconnect immediately; it must recur
	// maxConsecutiveFailures times. checkNodes returns early once the client is
	// nil, so re-arm it before each pass.
	for range maxConsecutiveFailures {
		a.consul = &fakeConsul{getErr: errors.New("internal server error")}
		a.checkNodes(context.Background())
	}

	if a.consulState != stateDisconnected {
		t.Errorf("expected disconnected after %d failures, got %v", maxConsecutiveFailures, a.consulState)
	}
}

// --- restartNode ----------------------------------------------------------

func TestRestartNode_DryRunSkipsOCI(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.cfg.DryRun = true
	oci := &fakeOCI{}
	a.oci = oci
	a.ociState = stateConnected

	a.restartWg.Add(1)
	a.restartNode(context.Background(), node)

	if oci.restartCount() != 0 {
		t.Errorf("dry-run should not call OCI, got %d calls", oci.restartCount())
	}
	if _, restarting := a.restarting["n1"]; restarting {
		t.Error("expected restarting flag cleared after restartNode")
	}
}

func TestRestartNode_OCIErrorTracksFailure(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	oci := &fakeOCI{err: errors.New("dial tcp: connection refused")}
	a.oci = oci
	a.ociState = stateConnected

	a.restartWg.Add(1)
	a.restartNode(context.Background(), node)

	// A connection error on restart marks OCI disconnected.
	if a.ociState != stateDisconnected {
		t.Errorf("expected oci disconnected after connection error, got %v", a.ociState)
	}
}

func TestRestartNode_DisconnectedOCISkips(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	oci := &fakeOCI{}
	a.oci = oci
	a.ociState = stateDisconnected // not connected

	a.restartWg.Add(1)
	a.restartNode(context.Background(), node)

	if oci.restartCount() != 0 {
		t.Errorf("expected no restart when OCI disconnected, got %d", oci.restartCount())
	}
}

// --- tick / wan-dns -------------------------------------------------------

func TestTick_ConnectedChecksNodes(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	// Already connected: tick should skip reconnect and run a node check.
	a.consul = &fakeConsul{} // missing pair
	a.consulState = stateConnected
	a.ociState = stateConnected
	a.oci = &fakeOCI{}

	a.tick(context.Background())
	a.restartWg.Wait()

	// A missing node on first sighting is recorded by checkNodes.
	if _, ok := a.missingSince["n1"]; !ok {
		t.Error("expected tick to run checkNodes and record the missing node")
	}
}

func TestTick_DisconnectedReconnects(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.consulState = stateDisconnected
	a.ociState = stateDisconnected
	a.newConsul = func(string) (ConsulClient, error) {
		return &fakeConsul{pairs: map[string]*consul.KVPair{nodeKey("n1"): {Key: nodeKey("n1")}}}, nil
	}
	a.newOCI = func(string, string) (InstanceRestarter, error) { return &fakeOCI{}, nil }

	a.tick(context.Background())

	if a.consulState != stateConnected {
		t.Errorf("expected tick to reconnect consul, got %v", a.consulState)
	}
	if a.ociState != stateConnected {
		t.Errorf("expected tick to reconnect oci, got %v", a.ociState)
	}
}

func TestStartWanDNSUpdater_DisabledIsNoop(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.cfg.WanDNS.Enabled = false

	// Disabled: must return immediately without starting a goroutine or panicking.
	a.startWanDNSUpdater(context.Background())
}

func TestStartWanDNSUpdater_ConstructionErrorIsLoggedNotFatal(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)

	// Enabled, but the token env var is unset so wandns.New fails with
	// ErrTokenMissing. The agent must swallow the error and continue - DDNS is
	// optional and must not start a goroutine or block the monitor loop.
	a.cfg.WanDNS.Enabled = true
	a.cfg.WanDNS.Cloudflare.APITokenEnv = "ORACLE_WATCHDOG_TEST_UNSET_TOKEN"
	t.Setenv("ORACLE_WATCHDOG_TEST_UNSET_TOKEN", "")

	// Must not panic and must return without launching the updater.
	a.startWanDNSUpdater(context.Background())
}

// --- handleMissingNode ----------------------------------------------------

func TestHandleMissingNode_WithinTimeoutDoesNotRestart(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}
	a := testAgent(node)
	a.cfg.Timeout = time.Hour
	oci := &fakeOCI{}
	a.oci = oci
	a.ociState = stateConnected

	now := time.Now()
	// Already seen missing, but only briefly - well within the timeout.
	a.missingSince["n1"] = now.Add(-time.Minute)

	a.handleMissingNode(context.Background(), node, now)
	a.restartWg.Wait()

	if oci.restartCount() != 0 {
		t.Errorf("expected no restart within timeout, got %d", oci.restartCount())
	}
	if _, ok := a.missingSince["n1"]; !ok {
		t.Error("expected missingSince retained while waiting out the timeout")
	}
}

// --- connection management ------------------------------------------------

func TestTryConnectConsul_SuccessAndFailure(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}

	t.Run("success", func(t *testing.T) {
		a := testAgent(node)
		a.newConsul = func(string) (ConsulClient, error) { return &fakeConsul{}, nil }
		a.tryConnectConsul()
		if a.consulState != stateConnected {
			t.Errorf("expected connected, got %v", a.consulState)
		}
	})

	t.Run("factory error", func(t *testing.T) {
		a := testAgent(node)
		a.newConsul = func(string) (ConsulClient, error) { return nil, errors.New("boom") }
		a.tryConnectConsul()
		if a.consulState != stateDisconnected {
			t.Errorf("expected disconnected on factory error, got %v", a.consulState)
		}
	})

	t.Run("leader unreachable", func(t *testing.T) {
		a := testAgent(node)
		a.newConsul = func(string) (ConsulClient, error) {
			return &fakeConsul{leaderErr: errors.New("no leader")}, nil
		}
		a.tryConnectConsul()
		if a.consulState != stateDisconnected {
			t.Errorf("expected disconnected when leader unreachable, got %v", a.consulState)
		}
	})
}

// --- option injection -----------------------------------------------------

func TestWithClientFactories_OverrideDefaults(t *testing.T) {
	cfg := &config.Config{
		Timeout:       5 * time.Minute,
		CheckInterval: 30 * time.Second,
		ConsulAddress: "localhost:8500",
		Nodes:         []config.NodeConfig{{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}},
	}

	var consulCalled, ociCalled bool
	a := New(cfg,
		WithConsulClientFactory(func(string) (ConsulClient, error) {
			consulCalled = true
			return &fakeConsul{}, nil
		}),
		WithOCIClientFactory(func(string, string) (InstanceRestarter, error) {
			ociCalled = true
			return &fakeOCI{}, nil
		}),
	)

	// The injected factories must replace the production ones and be invoked
	// when the agent establishes its connections.
	a.tryConnectConsul()
	a.tryConnectOCI()

	if !consulCalled {
		t.Error("expected injected consul factory to be used")
	}
	if !ociCalled {
		t.Error("expected injected oci factory to be used")
	}
	if a.consulState != stateConnected || a.ociState != stateConnected {
		t.Errorf("expected both connected, got consul=%v oci=%v", a.consulState, a.ociState)
	}
}

func TestTryConnectOCI_SuccessAndFailure(t *testing.T) {
	node := config.NodeConfig{Name: "n1", InstanceID: "i-1", CompartmentID: "c-1"}

	t.Run("success", func(t *testing.T) {
		a := testAgent(node)
		a.newOCI = func(string, string) (InstanceRestarter, error) { return &fakeOCI{}, nil }
		a.tryConnectOCI()
		if a.ociState != stateConnected {
			t.Errorf("expected connected, got %v", a.ociState)
		}
	})

	t.Run("factory error", func(t *testing.T) {
		a := testAgent(node)
		a.newOCI = func(string, string) (InstanceRestarter, error) { return nil, errors.New("boom") }
		a.tryConnectOCI()
		if a.ociState != stateDisconnected {
			t.Errorf("expected disconnected on factory error, got %v", a.ociState)
		}
	})
}
