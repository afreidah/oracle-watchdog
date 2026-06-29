// -------------------------------------------------------------------------------
// Oracle Watchdog - Monitor Integration Test
//
// Author: Alex Freidah
//
// Drives a real Monitor against a real Consul container and asserts the
// observable side effects through Consul's own API: a session-locked heartbeat
// key appears while the monitor runs, and is released when it shuts down (the
// session uses delete behaviour, exactly as the agent relies on in production).
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	consul "github.com/hashicorp/consul/api"

	"github.com/afreidah/oracle-watchdog/internal/monitor"
)

func TestMonitor_SessionLifecycle(t *testing.T) {
	addr, client := startConsul(t)

	const node = "integration-node"
	key := "oracle-watchdog/nodes/" + node

	// The monitor reads CONSUL_HTTP_ADDR at construction.
	t.Setenv("CONSUL_HTTP_ADDR", addr)

	ctx, cancel := context.WithCancel(context.Background())
	m := monitor.New(node)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Run(ctx)
	}()

	// The heartbeat key should appear, locked to an active session. The monitor
	// connects on its first tick (renew interval), so allow generous time.
	eventually(t, 40*time.Second, "heartbeat key acquired with a session", func() bool {
		pair, _, err := client.KV().Get(key, nil)
		return err == nil && pair != nil && pair.Session != ""
	})

	// Capture the session so we can confirm it is destroyed on shutdown.
	pair, _, err := client.KV().Get(key, nil)
	if err != nil || pair == nil {
		t.Fatalf("expected heartbeat key present: pair=%v err=%v", pair, err)
	}
	sessionID := pair.Session

	// Shut the monitor down; cleanup destroys the session, and delete-behaviour
	// removes the locked key.
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("monitor did not stop within 10s of cancellation")
	}

	eventually(t, 10*time.Second, "session destroyed on shutdown", func() bool {
		se, _, err := client.Session().Info(sessionID, nil)
		return err == nil && se == nil
	})

	// The session-locked key should be gone (delete behaviour).
	pair, _, err = client.KV().Get(key, &consul.QueryOptions{})
	if err != nil {
		t.Fatalf("kv get after shutdown: %v", err)
	}
	if pair != nil && pair.Session != "" {
		t.Errorf("expected heartbeat key released after shutdown, still locked to %s", pair.Session)
	}
}
