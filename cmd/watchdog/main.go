// -------------------------------------------------------------------------------
// Oracle Watchdog - Main Entry Point
//
// Author: Alex Freidah
//
// Monitors Oracle Cloud free-tier nodes and restarts them when unresponsive.
// Oracle periodically reclaims free-tier instances, which can leave them in a
// stuck state requiring a full stop/start cycle to recover.
//
// Modes:
//   - monitor: Runs on Oracle nodes, maintains Consul session heartbeat. With
//     a config file, also runs the WireGuard endpoint resolver.
//   - agent:   Runs on homelab, watches sessions and triggers OCI stop/start.
//     With wan_dns enabled, also keeps a Cloudflare DNS record in sync with
//     the home WAN IP.
// -------------------------------------------------------------------------------

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/afreidah/oracle-watchdog/internal/agent"
	"github.com/afreidah/oracle-watchdog/internal/config"
	"github.com/afreidah/oracle-watchdog/internal/monitor"
	"github.com/afreidah/oracle-watchdog/internal/tracing"
)

// defaultConfigPath is where both monitor and agent modes look for the YAML
// configuration file when -config is not specified. Monitor mode treats the
// file as optional; agent mode requires it.
const defaultConfigPath = "/etc/oracle-watchdog/config.yaml"

// main parses CLI flags and dispatches to the requested mode.
func main() {
	mode := flag.String("mode", "", "Run mode: 'monitor' or 'agent'")
	configPath := flag.String("config", defaultConfigPath, "Path to config file")
	nodeName := flag.String("node", "", "Node name for heartbeat (monitor mode, defaults to hostname)")
	enableTracing := flag.Bool("tracing", false, "Enable OpenTelemetry tracing")
	dryRun := flag.Bool("dry-run", false, "Log restart actions without executing them (agent mode)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Graceful shutdown on SIGINT/SIGTERM ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("shutdown signal received")
		cancel()
	}()

	switch *mode {
	case "monitor":
		runMonitor(ctx, *configPath, *nodeName, *enableTracing)
	case "agent":
		runAgent(ctx, *configPath, *dryRun, *enableTracing)
	default:
		slog.Error("invalid mode", "mode", *mode, "valid", []string{"monitor", "agent"})
		flag.Usage()
		os.Exit(1)
	}
}

// -------------------------------------------------------------------------
// TRACING
// -------------------------------------------------------------------------

// startTracing initializes tracing when the config enables it or the -tracing
// override is set, returning a stop function that is always safe to defer
// (a no-op when tracing is disabled or initialization failed).
func startTracing(ctx context.Context, mode string, cfg config.TracingConfig, force bool) func() {
	if !cfg.Enabled && !force {
		return func() {}
	}

	shutdown, err := tracing.Init(ctx, mode, cfg.Endpoint)
	if err != nil {
		slog.Warn("failed to initialize tracing, continuing without", "error", err)
		return func() {}
	}

	return func() {
		if err := shutdown(ctx); err != nil {
			slog.Warn("tracing shutdown error", "error", err)
		}
	}
}

// -------------------------------------------------------------------------
// MODE RUNNERS
// -------------------------------------------------------------------------

// runMonitor loads optional monitor-mode config and starts the heartbeat loop
// (plus the WireGuard endpoint resolver when configured).
func runMonitor(ctx context.Context, configPath, nodeName string, forceTracing bool) {
	if nodeName == "" {
		var err error
		nodeName, err = os.Hostname()
		if err != nil {
			slog.Error("failed to get hostname", "error", err)
			os.Exit(1)
		}
	}

	cfg, err := config.LoadMonitor(configPath)
	if err != nil {
		slog.Error("failed to load monitor config", "path", configPath, "error", err)
		os.Exit(1)
	}

	defer startTracing(ctx, "monitor", cfg.Tracing, forceTracing)()

	slog.Info("starting monitor mode",
		"node", nodeName,
		"wireguard_enabled", cfg.Wireguard.Enabled,
	)

	m := monitor.New(nodeName, monitor.WithWireguard(cfg.Wireguard))

	if err := m.Run(ctx); err != nil {
		slog.Error("monitor error", "error", err)
		os.Exit(1)
	}
}

// runAgent loads required agent-mode config and starts the agent loop (plus
// the WAN DNS updater when configured).
func runAgent(ctx context.Context, configPath string, dryRun, forceTracing bool) {
	cfg, err := config.LoadAgent(configPath)
	if err != nil {
		slog.Error("failed to load agent config", "path", configPath, "error", err)
		os.Exit(1)
	}

	cfg.DryRun = dryRun

	defer startTracing(ctx, "agent", cfg.Tracing, forceTracing)()

	slog.Info("starting agent mode",
		"nodes", len(cfg.Nodes),
		"timeout", cfg.Timeout,
		"dry_run", dryRun,
		"wan_dns_enabled", cfg.WanDNS.Enabled,
	)

	a := agent.New(cfg)

	if err := a.Run(ctx); err != nil {
		slog.Error("agent error", "error", err)
		os.Exit(1)
	}
}
