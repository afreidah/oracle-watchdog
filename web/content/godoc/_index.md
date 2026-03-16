---
title: "oracle-watchdog Go API"
linkTitle: "Go API"
weight: 30
chapter: true
---

<div class="landing-subheader">Auto-generated reference documentation from the Go source code.</div>

<div class="landing-grid">
  <a class="landing-card" href="agent/">
    <div>
      <strong>agent</strong>
      <p>Agent mode: monitors Consul for missing Oracle node sessions and orchestrates OCI restart cycles.</p>
    </div>
  </a>
  <a class="landing-card" href="monitor/">
    <div>
      <strong>monitor</strong>
      <p>Monitor mode: maintains Consul session heartbeat on each Oracle node with state machine lifecycle.</p>
    </div>
  </a>
  <a class="landing-card" href="config/">
    <div>
      <strong>config</strong>
      <p>YAML configuration loading and validation for agent mode node mappings and timeouts.</p>
    </div>
  </a>
  <a class="landing-card" href="oci/">
    <div>
      <strong>oci</strong>
      <p>OCI SDK wrapper for instance stop/start lifecycle with state polling and timeout handling.</p>
    </div>
  </a>
  <a class="landing-card" href="metrics/">
    <div>
      <strong>metrics</strong>
      <p>Prometheus metric definitions for monitor and agent modes: connection status, session health, restart tracking.</p>
    </div>
  </a>
  <a class="landing-card" href="tracing/">
    <div>
      <strong>tracing</strong>
      <p>OpenTelemetry tracer setup, client span helpers, and attribute constructors for Tempo integration.</p>
    </div>
  </a>
</div>
