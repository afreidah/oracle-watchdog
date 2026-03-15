---
title: "Architecture"
weight: 5
---

<p class="landing-subheader">Distributed heartbeat monitoring and automatic OCI instance recovery</p>

<style>
  #arch-diagram { cursor: default; }
  #arch-diagram .node { cursor: pointer; }
  #arch-tooltip {
    display: none;
    position: fixed;
    background: #1e293b;
    border: 1px solid #f59e0b;
    border-radius: 8px;
    padding: 1rem 1.25rem;
    color: #e2e8f0;
    font-size: 0.9rem;
    line-height: 1.6;
    max-width: 360px;
    z-index: 1000;
    box-shadow: 0 8px 32px rgba(0, 0, 0, 0.6);
    pointer-events: none;
  }
  #arch-tooltip strong {
    color: #f59e0b;
    font-size: 1rem;
  }
  #arch-tooltip .detail {
    margin-top: 0.5rem;
    color: #94a3b8;
  }
</style>

<div id="arch-diagram"></div>
<div id="arch-tooltip"></div>

<script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
<script>
(function() {
  var diagramSrc = [
    'flowchart LR',
    '    ON1["Oracle Node 1<br><small>monitor mode</small>"]',
    '    ON2["Oracle Node 2<br><small>monitor mode</small>"]',
    '    ON3["Oracle Node 3<br><small>monitor mode</small>"]',
    '    ON4["Oracle Node 4<br><small>monitor mode</small>"]',
    '    CONSUL["Consul<br><small>KV + Sessions</small>"]',
    '    AGENT["oracle-watchdog<br><small>agent mode</small>"]',
    '    OCI["Oracle Cloud API<br><small>stop / start</small>"]',
    '    PROM["Prometheus<br><small>metrics</small>"]',
    '    TEMPO["Tempo<br><small>tracing</small>"]',
    '    GRAFANA["Grafana<br><small>dashboards</small>"]',
    '',
    '    ON1 -->|"heartbeat"| CONSUL',
    '    ON2 -->|"heartbeat"| CONSUL',
    '    ON3 -->|"heartbeat"| CONSUL',
    '    ON4 -->|"heartbeat"| CONSUL',
    '    CONSUL -->|"poll missing sessions"| AGENT',
    '    AGENT -->|"stop/start"| OCI',
    '    ON1 -->|"/metrics"| PROM',
    '    ON2 -->|"/metrics"| PROM',
    '    ON3 -->|"/metrics"| PROM',
    '    ON4 -->|"/metrics"| PROM',
    '    AGENT -->|"/metrics"| PROM',
    '    AGENT -->|"OTLP gRPC"| TEMPO',
    '    PROM --> GRAFANA',
    '    TEMPO --> GRAFANA',
    '',
    '    classDef oracle fill:#2d1f0a,stroke:#f59e0b,color:#fef3c7',
    '    classDef infra fill:#1e293b,stroke:#94a3b8,color:#e2e8f0',
    '    classDef cloud fill:#0c2d48,stroke:#38bdf8,color:#e0f2fe',
    '    classDef sink fill:#132a1f,stroke:#22c55e,color:#dcfce7',
    '    classDef viz fill:#2d2513,stroke:#f59e0b,color:#fef3c7',
    '',
    '    class ON1,ON2,ON3,ON4 oracle',
    '    class CONSUL,AGENT infra',
    '    class OCI cloud',
    '    class PROM,TEMPO sink',
    '    class GRAFANA viz'
  ].join('\n');

  mermaid.initialize({
    startOnLoad: false,
    theme: 'dark',
    flowchart: { nodeSpacing: 18, rankSpacing: 30, curve: 'basis', padding: 8, diagramPadding: 8, useMaxWidth: true }
  });

  mermaid.render('arch-mermaid-svg', diagramSrc).then(function(result) {
    document.getElementById('arch-diagram').innerHTML = result.svg;
    wireUpInteractivity();
  });

  var nodeInfo = {
    'ON1':     { title: 'Oracle Node (Monitor Mode)', detail: 'Runs as a systemd service on each Oracle Cloud node. Creates a Consul session with 30s TTL and renews it every 10 seconds. Writes a KV pair locked to the session at oracle-watchdog/nodes/{nodename}. If the node is reclaimed by Oracle, the session expires and the KV pair is automatically deleted.' },
    'ON2':     { title: 'Oracle Node (Monitor Mode)', detail: 'Each node independently maintains its own Consul session. State machine: disconnected → connecting → active. Deployed via Debian packages and Ansible.' },
    'ON3':     { title: 'Oracle Node (Monitor Mode)', detail: 'Exposes Prometheus metrics on port 9104 for connection status, session health, and renewal rates.' },
    'ON4':     { title: 'Oracle Node (Monitor Mode)', detail: 'All monitors share the same binary, differentiated by hostname. Monitors never crash — they continuously retry on Consul unavailability.' },
    'CONSUL':  { title: 'Consul (KV + Sessions)', detail: 'Stores session heartbeats as KV pairs at oracle-watchdog/nodes/{nodename}. Sessions use delete behavior — when a session expires (node unresponsive for 30s), the associated KV pair is automatically removed. The agent polls these keys to detect missing nodes.' },
    'AGENT':   { title: 'Oracle Watchdog Agent', detail: 'Runs on homelab infrastructure as a Nomad job. Polls Consul on a configurable interval (default 120s) for missing node KV pairs. When a node has been absent longer than the timeout (default 900s), triggers an OCI stop/start cycle. Tracks consecutive restart attempts per node, resets on recovery.' },
    'OCI':     { title: 'Oracle Cloud API', detail: 'OCI Compute API for instance lifecycle management. The agent issues a stop command, polls instance state until STOPPED (10s intervals, 5m timeout), then issues a start command and polls until RUNNING. Requires instance-action (STOP, START) and instance-read IAM permissions.' },
    'PROM':    { title: 'Prometheus', detail: 'Scrapes monitor metrics on port 9104 and agent metrics on port 9105. 13 metric families covering connection status, session health, renewals, failures, reconnects, node counts, per-node restart counters, and check failures.' },
    'TEMPO':   { title: 'Tempo', detail: 'Receives OpenTelemetry traces from the agent via OTLP gRPC. Each restart cycle creates a trace with spans for node detection, OCI stop, state polling, and OCI start. Includes node name, instance ID, and error details.' },
    'GRAFANA': { title: 'Grafana', detail: 'Displays the oracle-watchdog dashboard with monitor node status table, agent connection panels, restart activity tracking, and log panels from Loki for both monitor and agent processes.' }
  };

  function wireUpInteractivity() {
    var tooltip = document.getElementById('arch-tooltip');
    document.querySelectorAll('#arch-diagram .node').forEach(function(node) {
      var id = node.id || '';
      var key = id.replace(/^flowchart-/, '').replace(/-\d+$/, '');
      if (!nodeInfo[key]) return;

      node.addEventListener('mouseenter', function(e) {
        var info = nodeInfo[key];
        tooltip.innerHTML = '<strong>' + info.title + '</strong><div class="detail">' + info.detail + '</div>';
        tooltip.style.display = 'block';
      });
      node.addEventListener('mousemove', function(e) {
        tooltip.style.left = (e.clientX + 16) + 'px';
        tooltip.style.top = (e.clientY + 16) + 'px';
      });
      node.addEventListener('mouseleave', function() {
        tooltip.style.display = 'none';
      });
    });
  }
})();
</script>

## How It Works

### Monitor Mode (Oracle Nodes)

1. Each Oracle node runs the monitor as a **systemd service**
2. Monitor creates a **Consul session** with 30-second TTL and `delete` behavior
3. A **KV pair** is written at `oracle-watchdog/nodes/{nodename}`, locked to the session
4. The session is **renewed every 10 seconds** — if renewal fails, the monitor reconnects automatically
5. If a node becomes unresponsive (reclaimed by Oracle), the session expires and the **KV pair is deleted**

### Agent Mode (Homelab)

1. The agent runs as a **Nomad job** on homelab infrastructure
2. On each check interval (default 120s), it **polls Consul** for missing node KV pairs
3. When a node has been absent longer than the **timeout** (default 900s), it triggers a restart:
   - Issues an **OCI stop** command
   - Polls instance state until **STOPPED** (10s intervals, 5m max wait)
   - Issues an **OCI start** command
   - Polls instance state until **RUNNING**
4. **Consecutive restart attempts** are tracked per node and reset when the node recovers
5. **Duplicate restart prevention** ensures only one restart is in-flight per node at a time

### Safety Features

- Configurable **max restart attempts** per node (0 = unlimited)
- **Dry-run mode** for testing (`-dry-run` flag)
- **Connection health tracking** with consecutive failure thresholds for both Consul and OCI
- Automatic **connection state machine** transitions — never crashes, always retries
