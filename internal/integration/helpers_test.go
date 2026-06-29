// -------------------------------------------------------------------------------
// Oracle Watchdog - Integration Test Helpers
//
// Author: Alex Freidah
//
// Shared setup for integration tests. Uses testcontainers to spin up a real
// Consul agent automatically - no external docker-compose required, just
// `go test -tags integration ./internal/integration/`. Tests that reach Consul
// exercise the real SDK adapters (the same code paths the binary runs in
// production), which the fakes-based unit tests deliberately bypass.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"
	tcconsul "github.com/testcontainers/testcontainers-go/modules/consul"
)

// consulImage pins the Consul image used for integration runs.
const consulImage = "hashicorp/consul:1.20"

// startConsul launches a Consul container and returns its HTTP API address
// (host:port) plus a ready-to-use SDK client. The container is terminated via
// t.Cleanup.
func startConsul(t *testing.T) (string, *consul.Client) {
	t.Helper()

	ctx := context.Background()
	container, err := tcconsul.Run(ctx, consulImage)
	if err != nil {
		t.Fatalf("start consul container: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	addr, err := container.ApiEndpoint(ctx)
	if err != nil {
		t.Fatalf("consul api endpoint: %v", err)
	}

	cfg := consul.DefaultConfig()
	cfg.Address = addr
	client, err := consul.NewClient(cfg)
	if err != nil {
		t.Fatalf("consul client: %v", err)
	}

	// Wait until the agent has elected a leader before handing the address out.
	waitForLeader(t, client)

	return addr, client
}

// waitForLeader polls until Consul reports a Raft leader (the same probe the
// monitor/agent use), so tests don't race container startup.
func waitForLeader(t *testing.T, client *consul.Client) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if leader, err := client.Status().Leader(); err == nil && leader != "" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("consul did not elect a leader within 30s")
}

// eventually polls cond until it returns true or the timeout elapses, failing
// the test with msg otherwise.
func eventually(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}
