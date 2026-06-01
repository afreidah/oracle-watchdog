// -------------------------------------------------------------------------------
// Main Entry Point Tests
//
// Author: Alex Freidah
//
// Tests the startTracing gating helper: tracing initializes only when the
// config enables it or the -tracing override is set, and the returned stop
// function is always safe to call.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"testing"

	"github.com/afreidah/oracle-watchdog/internal/config"
)

// -------------------------------------------------------------------------
// START TRACING
// -------------------------------------------------------------------------

// TestStartTracing_DisabledNoForce verifies tracing stays off when neither the
// config nor the override enables it, and the returned stop func is a safe
// no-op.
func TestStartTracing_DisabledNoForce(t *testing.T) {
	stop := startTracing(context.Background(), "agent", config.TracingConfig{Enabled: false}, false)
	if stop == nil {
		t.Fatal("startTracing returned a nil stop func")
	}
	stop() // no-op path must not panic
}

// TestStartTracing_ForceOverride verifies the -tracing override initializes
// tracing even when the config disables it, returning a working stop func.
func TestStartTracing_ForceOverride(t *testing.T) {
	stop := startTracing(context.Background(), "agent", config.TracingConfig{Enabled: false}, true)
	if stop == nil {
		t.Fatal("startTracing returned a nil stop func")
	}
	stop() // shuts the installed provider down; must not panic
}

// TestStartTracing_ConfigEnabled verifies config-enabled tracing initializes
// without needing the override.
func TestStartTracing_ConfigEnabled(t *testing.T) {
	stop := startTracing(context.Background(), "monitor",
		config.TracingConfig{Enabled: true, Endpoint: "tempo.service.consul:4318"}, false)
	if stop == nil {
		t.Fatal("startTracing returned a nil stop func")
	}
	stop()
}
