// -------------------------------------------------------------------------------
// Oracle Watchdog - Agent Integration Test
//
// Author: Alex Freidah
//
// Drives a real Agent against a real Consul container. The Consul interaction
// is genuine; only the OCI restart is substituted (an injected fake), so the
// test exercises the full missing-node detection pipeline up to and including
// the restart decision without touching Oracle Cloud.
//
// Two scenarios:
//   - a node whose heartbeat key is present is treated as alive (no restart);
//   - a node whose key is absent past the timeout triggers a restart.
//
// The restart is run in dry-run mode, so success is asserted via the restart
// metric rather than a real OCI call (the injected client only serves to make
// the agent consider OCI "connected").
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/oracle-watchdog/internal/agent"
	"github.com/afreidah/oracle-watchdog/internal/config"
	"github.com/afreidah/oracle-watchdog/internal/metrics"
)

// fakeOCI is an injected agent.InstanceRestarter. In dry-run the agent never
// calls it; it exists so the agent's OCI connection succeeds.
type fakeOCI struct{}

func (fakeOCI) RestartInstance(_ context.Context, _, _ string) error { return nil }

func TestAgent_DetectsMissingNodeAndRestarts(t *testing.T) {
	addr, client := startConsul(t)

	const (
		missingNode = "missing-node"
		aliveNode   = "alive-node"
	)

	// Keep an "alive" node's heartbeat key present for the duration via a
	// long-lived session, so the agent should never restart it.
	sessionID, _, err := client.Session().Create(&consul.SessionEntry{
		Name: "integration-alive",
		TTL:  "60s",
	}, nil)
	if err != nil {
		t.Fatalf("create alive session: %v", err)
	}
	acquired, _, err := client.KV().Acquire(&consul.KVPair{
		Key:     "oracle-watchdog/nodes/" + aliveNode,
		Value:   []byte("alive"),
		Session: sessionID,
	}, nil)
	if err != nil || !acquired {
		t.Fatalf("acquire alive key: acquired=%v err=%v", acquired, err)
	}

	cfg := &config.Config{
		ConsulAddress: addr,
		CheckInterval: 300 * time.Millisecond,
		Timeout:       300 * time.Millisecond,
		DryRun:        true, // assert via metrics, no real OCI call
		Nodes: []config.NodeConfig{
			{Name: missingNode, InstanceID: "ocid-missing", CompartmentID: "comp"},
			{Name: aliveNode, InstanceID: "ocid-alive", CompartmentID: "comp"},
		},
	}

	a := agent.New(cfg, agent.WithOCIClientFactory(
		func(string, string) (agent.InstanceRestarter, error) { return fakeOCI{}, nil },
	))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx) }()

	// The missing node has no heartbeat key, so once it has been absent past the
	// timeout the agent triggers a (dry-run) restart, counted as a success.
	eventually(t, 30*time.Second, "missing node restart attempted", func() bool {
		return testutil.ToFloat64(metrics.AgentRestartSuccesses.WithLabelValues(missingNode)) >= 1
	})

	// The alive node held its session-locked key throughout and must never have
	// been restarted.
	if got := testutil.ToFloat64(metrics.AgentRestartAttempts.WithLabelValues(aliveNode)); got != 0 {
		t.Errorf("alive node should not be restarted, got %v attempts", got)
	}
}
