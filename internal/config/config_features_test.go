// -------------------------------------------------------------------------------
// Oracle Watchdog - Configuration Feature-Block Tests
//
// Project: Munchbox / Author: Alex Freidah
//
// Covers the validation branches for the optional wireguard and wan_dns
// feature blocks plus the LoadMonitor entry point.
// -------------------------------------------------------------------------------

package config

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempConfig creates a temp file containing content and returns the path.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// -------------------------------------------------------------------------
// LOAD MONITOR
// -------------------------------------------------------------------------

// TestLoadMonitor_MissingFileReturnsDefaults verifies the optional-config
// behaviour of monitor mode: an absent file is not an error.
func TestLoadMonitor_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := LoadMonitor(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if cfg.Wireguard.Enabled {
		t.Error("expected wireguard disabled by default")
	}
	if cfg.Wireguard.Interface != "wg0" {
		t.Errorf("expected default interface wg0, got %q", cfg.Wireguard.Interface)
	}
}

// TestLoadMonitor_DisabledWireguardSkipsValidation verifies that when the
// wireguard block exists but enabled is false, missing fields do not error.
func TestLoadMonitor_DisabledWireguardSkipsValidation(t *testing.T) {
	p := writeTempConfig(t, `
wireguard:
  enabled: false
`)
	if _, err := LoadMonitor(p); err != nil {
		t.Fatalf("expected nil error for disabled block, got %v", err)
	}
}

// TestLoadMonitor_EnabledWireguardValidates verifies the validation path
// surfaces missing required fields when the block is enabled.
func TestLoadMonitor_EnabledWireguardValidates(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "missing peer pubkey",
			content: validWireguardYAML(map[string]string{"peer_pubkey": ""}),
			wantErr: "peer_pubkey is required",
		},
		{
			name:    "missing endpoint hostname",
			content: validWireguardYAML(map[string]string{"endpoint_hostname": ""}),
			wantErr: "endpoint_hostname is required",
		},
		{
			name:    "out of range port",
			content: validWireguardYAML(map[string]string{"endpoint_port": "70000"}),
			wantErr: "endpoint_port must be 1-65535",
		},
		{
			name:    "resolve interval too short",
			content: validWireguardYAML(map[string]string{"resolve_interval": "5s"}),
			wantErr: "resolve_interval must be at least 10s",
		},
		{
			name:    "stale threshold too short",
			content: validWireguardYAML(map[string]string{"stale_handshake_threshold": "10s"}),
			wantErr: "stale_handshake_threshold must be at least 30s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTempConfig(t, tc.content)
			_, err := LoadMonitor(p)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestLoadMonitor_HappyPath verifies a fully-valid wireguard block loads.
func TestLoadMonitor_HappyPath(t *testing.T) {
	p := writeTempConfig(t, validWireguardYAML(nil))
	cfg, err := LoadMonitor(p)
	if err != nil {
		t.Fatalf("LoadMonitor: %v", err)
	}
	if !cfg.Wireguard.Enabled {
		t.Error("expected wireguard enabled")
	}
	if cfg.Wireguard.EndpointPort != 51820 {
		t.Errorf("expected port 51820, got %d", cfg.Wireguard.EndpointPort)
	}
	if cfg.Wireguard.ResolveInterval != time.Minute {
		t.Errorf("expected resolve_interval 1m, got %v", cfg.Wireguard.ResolveInterval)
	}
}

// -------------------------------------------------------------------------
// WAN DNS VALIDATION
// -------------------------------------------------------------------------

// TestLoadAgent_DisabledWanDNSSkipsValidation verifies that an agent config
// without wan_dns enabled does not require its fields to be set.
func TestLoadAgent_DisabledWanDNSSkipsValidation(t *testing.T) {
	p := writeTempConfig(t, validAgentYAML())
	if _, err := LoadAgent(p); err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
}

// TestLoadAgent_EnabledWanDNSValidates verifies missing required fields
// surface as validation errors when wan_dns is enabled.
func TestLoadAgent_EnabledWanDNSValidates(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{
			name: "missing hostname",
			extra: `
wan_dns:
  enabled: true
  cloudflare:
    zone_id: "z1"
`,
			wantErr: "hostname is required",
		},
		{
			name: "missing zone id",
			extra: `
wan_dns:
  enabled: true
  hostname: "wg.example.com"
`,
			wantErr: "cloudflare.zone_id is required",
		},
		{
			name: "no detection providers",
			extra: `
wan_dns:
  enabled: true
  hostname: "wg.example.com"
  cloudflare:
    zone_id: "z1"
  detection_providers: []
`,
			wantErr: "detection_providers must list at least one URL",
		},
		{
			name: "poll interval too short",
			extra: `
wan_dns:
  enabled: true
  hostname: "wg.example.com"
  cloudflare:
    zone_id: "z1"
  poll_interval: 10s
`,
			wantErr: "poll_interval must be at least 30s",
		},
		{
			name: "cooldown too short",
			extra: `
wan_dns:
  enabled: true
  hostname: "wg.example.com"
  cloudflare:
    zone_id: "z1"
  cooldown: 10s
`,
			wantErr: "cooldown must be at least 1m",
		},
		{
			name: "missing api token env",
			extra: `
wan_dns:
  enabled: true
  hostname: "wg.example.com"
  cloudflare:
    api_token_env: ""
    zone_id: "z1"
`,
			wantErr: "cloudflare.api_token_env is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeTempConfig(t, validAgentYAML()+tc.extra)
			_, err := LoadAgent(p)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// validWireguardYAML returns a complete wireguard config block. overrides
// replace specific fields in the rendered YAML so tests can exercise one
// invalid field at a time.
func validWireguardYAML(overrides map[string]string) string {
	defaults := map[string]string{
		"peer_pubkey":               "cUn1CNonnDVBMWg6jFop2htQ2GWu8jaepTt8/yPPcjI=",
		"endpoint_hostname":         "wg.example.com",
		"endpoint_port":             "51820",
		"resolve_interval":          "60s",
		"stale_handshake_threshold": "180s",
	}
	maps.Copy(defaults, overrides)
	return `
wireguard:
  enabled: true
  interface: wg0
  peer_pubkey: "` + defaults["peer_pubkey"] + `"
  endpoint_hostname: "` + defaults["endpoint_hostname"] + `"
  endpoint_port: ` + defaults["endpoint_port"] + `
  resolve_interval: ` + defaults["resolve_interval"] + `
  stale_handshake_threshold: ` + defaults["stale_handshake_threshold"] + `
`
}

// validAgentYAML returns a minimum valid agent config so wan_dns blocks can
// be appended without tripping unrelated validation rules.
func validAgentYAML() string {
	return `
timeout: 5m
nodes:
  - name: oracle-node-1
    instance_id: ocid1.instance.test
    compartment_id: ocid1.compartment.test
`
}

// -------------------------------------------------------------------------
// TRACING CONFIG
// -------------------------------------------------------------------------

// TestLoadMonitor_TracingParsed verifies the shared tracing block parses for
// monitor mode with both fields populated.
func TestLoadMonitor_TracingParsed(t *testing.T) {
	p := writeTempConfig(t, `
tracing:
  enabled: true
  endpoint: "tempo.service.consul:4318"
`)
	cfg, err := LoadMonitor(p)
	if err != nil {
		t.Fatalf("LoadMonitor: %v", err)
	}
	if !cfg.Tracing.Enabled {
		t.Error("expected tracing enabled")
	}
	if cfg.Tracing.Endpoint != "tempo.service.consul:4318" {
		t.Errorf("endpoint = %q, want %q", cfg.Tracing.Endpoint, "tempo.service.consul:4318")
	}
}

// TestLoadAgent_TracingParsed verifies the shared tracing block parses for
// agent mode alongside the required agent fields.
func TestLoadAgent_TracingParsed(t *testing.T) {
	p := writeTempConfig(t, validAgentYAML()+`
tracing:
  enabled: true
  endpoint: "tempo.service.consul:4318"
`)
	cfg, err := LoadAgent(p)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if !cfg.Tracing.Enabled {
		t.Error("expected tracing enabled")
	}
	if cfg.Tracing.Endpoint != "tempo.service.consul:4318" {
		t.Errorf("endpoint = %q, want %q", cfg.Tracing.Endpoint, "tempo.service.consul:4318")
	}
}

// TestLoadMonitor_TracingDefaultsDisabled verifies an absent tracing block
// leaves tracing disabled with an empty endpoint, so resolution falls through
// to the env var or built-in default at Init time.
func TestLoadMonitor_TracingDefaultsDisabled(t *testing.T) {
	cfg, err := LoadMonitor(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadMonitor: %v", err)
	}
	if cfg.Tracing.Enabled {
		t.Error("expected tracing disabled by default")
	}
	if cfg.Tracing.Endpoint != "" {
		t.Errorf("expected empty endpoint by default, got %q", cfg.Tracing.Endpoint)
	}
}
