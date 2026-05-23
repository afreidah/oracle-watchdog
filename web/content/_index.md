---
title: "oracle-watchdog"
archetype: "home"
description: "Distributed monitoring and recovery system for Oracle Cloud free-tier instances"
---

<div style="text-align: center; margin-top: -2rem; margin-bottom: -2rem;">
  <img src="/images/logo.png" alt="oracle-watchdog" style="max-width: 750px; height: auto;">
</div>

<div class="badge-grid">

{{% badge style="primary" icon="fas fa-heartbeat" %}}Session Heartbeat{{% /badge %}}
{{% badge style="info" title=" " icon="fas fa-cloud" %}}OCI Stop/Start{{% /badge %}}
{{% badge style="danger" icon="fas fa-fire" %}}Prometheus Metrics{{% /badge %}}
{{% badge style="green" icon="fas fa-sync" %}}Self-Healing{{% /badge %}}
{{% badge style="warning" title=" " icon="fas fa-project-diagram" %}}OpenTelemetry Tracing{{% /badge %}}

</div>

<div style="text-align: center; margin-top: 1rem;">

{{% button href="docs/readme/" style="primary" icon="fas fa-book" %}}README{{% /button %}}
{{% button href="docs/architecture/" style="primary" icon="fas fa-project-diagram" %}}Architecture{{% /button %}}
{{% button href="godoc/" style="primary" icon="fas fa-code" %}}Go API{{% /button %}}
{{% button href="docs/grafana/" style="primary" icon="fas fa-chart-area" %}}Grafana{{% /button %}}
{{% button href="https://github.com/afreidah/oracle-watchdog" style="primary" icon="fab fa-github" %}}GitHub{{% /button %}}

</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #f59e0b;">Automatic recovery for Oracle Cloud free-tier nodes</h2>

Oracle periodically reclaims free-tier instances, leaving them in a stuck state that requires a full stop/start cycle to recover. Oracle Watchdog detects unresponsive nodes by polling Consul KV for session-locked heartbeats that expire when a node goes silent, then automatically triggers OCI restart cycles.

<div class="hero-bullets">

- **Monitor mode** runs on each Oracle node, holding a session-locked KV entry in Consul as its heartbeat signal
- **Agent mode** runs on infrastructure separate from the monitored nodes, polling Consul KV for missing heartbeats and orchestrating OCI stop/start cycles
- **Self-healing design** ensures the service never crashes due to Consul or OCI unavailability
- **OpenTelemetry tracing** provides visibility into restart cycles via Tempo

</div>

<hr style="margin-top: 3rem;">

<h2 style="text-align: center; color: #f59e0b;">Key Features</h2>

<div class="feature-grid">
  <div class="feature-item">
    <div>
      <strong>Consul Session Heartbeat</strong>
      <p>Monitor processes maintain a Consul session with 30s TTL on each Oracle node.</p>
    </div>
    <div class="feature-detail">Sessions are renewed every 10 seconds. A KV pair locked to the session is written at oracle-watchdog/nodes/{nodename}. When a node becomes unresponsive, the session expires and the KV pair is automatically deleted.</div>
  </div>
  <div class="feature-item">
    <div>
      <strong>Automatic OCI Recovery</strong>
      <p>Agent detects missing heartbeats and triggers OCI stop/start cycles to recover stuck instances.</p>
    </div>
    <div class="feature-detail">Configurable timeout before restart (default 5m). Issues OCI stop, polls until STOPPED, then issues start and polls until RUNNING. Tracks consecutive attempts per node and resets on recovery.</div>
  </div>
  <div class="feature-item">
    <div>
      <strong>Self-Healing Design</strong>
      <p>Never crashes due to Consul or OCI unavailability - continuously retries connections.</p>
    </div>
    <div class="feature-detail">Both monitor and agent modes use state machines that transition between disconnected, connecting, and active states. Consecutive failure tracking triggers connection resets. Duplicate restart prevention via in-flight tracking.</div>
  </div>
  <div class="feature-item">
    <div>
      <strong>Prometheus Metrics</strong>
      <p>13 metrics covering connection health, session status, and restart activity per node.</p>
    </div>
    <div class="feature-detail">Monitor exposes connection and session gauges, renewal/failure counters, and reconnect attempts on port 9104. Agent exposes connection status, node counts, per-node restart counters, and check failures on port 9105.</div>
  </div>
  <div class="feature-item">
    <div>
      <strong>OpenTelemetry Tracing</strong>
      <p>Every restart cycle is traced end-to-end with spans for OCI stop, poll, and start operations.</p>
    </div>
    <div class="feature-detail">Exports traces to Tempo via OTLP gRPC. Each trace captures node name, instance ID, timing for stop/start operations, and error details on failure.</div>
  </div>
  <div class="feature-item">
    <div>
      <strong>Flexible Deployment</strong>
      <p>One binary, two modes. Monitor ships as a systemd-friendly Debian package, agent ships as a Docker image.</p>
    </div>
    <div class="feature-detail">Same binary on both sides, selected by the -mode flag. Run monitors directly on each Oracle node and the agent anywhere that can reach Consul and the OCI API.</div>
  </div>
  <div class="feature-item">
    <div>
      <strong>Optional: WireGuard Endpoint Resolver</strong>
      <p>Monitor mode can re-resolve a configured WG peer hostname and refresh the kernel peer endpoint when its IP changes.</p>
    </div>
    <div class="feature-detail">Default-disabled and independent of the core OCI-restart flow. Forces an immediate re-resolve when the most recent peer handshake exceeds the configured staleness threshold. Updates the kernel via netlink (wgctrl).</div>
  </div>
  <div class="feature-item">
    <div>
      <strong>Optional: Cloudflare WAN-IP DDNS Updater</strong>
      <p>Agent mode can detect the host's public IPv4 and keep a Cloudflare A record in sync.</p>
    </div>
    <div class="feature-detail">Default-disabled and independent of the core OCI-restart flow. Polls configurable detection providers (ipify, Cloudflare trace) in order, IPv4 only. Cloudflare API token read once at startup from an env var.</div>
  </div>
</div>
