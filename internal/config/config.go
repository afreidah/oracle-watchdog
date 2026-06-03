// -------------------------------------------------------------------------------
// Oracle Watchdog - Configuration
//
// Author: Alex Freidah
//
// Loads and validates oracle-watchdog configuration from YAML. The same file
// serves both monitor and agent modes; each mode validates only the fields it
// needs. Optional feature blocks (wireguard, wan_dns) are inert when absent.
// -------------------------------------------------------------------------------

package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// -------------------------------------------------------------------------
// CONFIG TYPES
// -------------------------------------------------------------------------

// Config holds oracle-watchdog configuration shared by monitor and agent modes.
// Each mode reads only the fields relevant to its responsibilities and
// validates them via the matching LoadAgent or LoadMonitor entry point.
type Config struct {
	// Timeout defines how long an Oracle node must be unresponsive before the
	// agent triggers a restart cycle.
	Timeout time.Duration `yaml:"timeout"`

	// CheckInterval defines how often the agent scans Consul for missing
	// session keys.
	CheckInterval time.Duration `yaml:"check_interval"`

	// ConsulAddress is the host:port of the Consul HTTP API used by the agent.
	ConsulAddress string `yaml:"consul_address"`

	// OCI holds Oracle Cloud authentication settings used by the agent when
	// issuing instance-action API calls.
	OCI OCIConfig `yaml:"oci"`

	// Nodes lists the Oracle instances the agent monitors and is allowed to
	// restart. Required for agent mode.
	Nodes []NodeConfig `yaml:"nodes"`

	// MaxRestartAttempts caps consecutive restarts per node before the agent
	// stops trying. Zero means unlimited; the per-node counter resets on
	// recovery.
	MaxRestartAttempts int `yaml:"max_restart_attempts"`

	// DryRun, when true, logs restart actions without executing them. Set via
	// the agent CLI flag rather than the YAML file.
	DryRun bool `yaml:"-"`

	// Wireguard configures the monitor-side endpoint resolver. Default-disabled
	// so deploys without a wireguard block continue to behave as before.
	Wireguard WireguardConfig `yaml:"wireguard"`

	// WanDNS configures the agent-side WAN-IP DDNS updater. Default-disabled.
	WanDNS WanDNSConfig `yaml:"wan_dns"`

	// Tracing configures OpenTelemetry trace export. Shared by both modes and
	// default-disabled; the -tracing CLI flag force-enables regardless.
	Tracing TracingConfig `yaml:"tracing"`
}

// TracingConfig configures OpenTelemetry trace export. Shared by both modes.
type TracingConfig struct {
	// Enabled toggles tracer initialization. When false, and the -tracing CLI
	// override is unset, no tracer provider is installed.
	Enabled bool `yaml:"enabled"`

	// Endpoint is the OTLP/HTTP collector as a bare host:port with no scheme.
	// Empty falls back to OTEL_EXPORTER_OTLP_ENDPOINT, then a built-in default.
	Endpoint string `yaml:"endpoint"`
}

// OCIConfig holds Oracle Cloud authentication settings.
type OCIConfig struct {
	// ConfigPath is the filesystem path to an OCI SDK config file.
	ConfigPath string `yaml:"config_path"`

	// Profile is the named section within the OCI config file to use.
	Profile string `yaml:"profile"`
}

// NodeConfig maps a Consul session name to an OCI instance the agent restarts.
type NodeConfig struct {
	// Name is the Consul session/node name reported by the matching monitor.
	Name string `yaml:"name"`

	// InstanceID is the OCID of the OCI compute instance.
	InstanceID string `yaml:"instance_id"`

	// CompartmentID is the OCID of the OCI compartment containing the instance.
	CompartmentID string `yaml:"compartment_id"`
}

// WireguardConfig configures the monitor-side endpoint resolver. The resolver
// updates the kernel WireGuard peer endpoint when the configured hostname
// resolves to a new IP.
type WireguardConfig struct {
	// Enabled toggles the endpoint resolver. When false, the resolver does not
	// run and other fields are ignored.
	Enabled bool `yaml:"enabled"`

	// Interface is the WireGuard interface name (typically "wg0").
	Interface string `yaml:"interface"`

	// PeerPubkey is the base64-encoded public key of the remote peer to track.
	PeerPubkey string `yaml:"peer_pubkey"`

	// EndpointHostname is the DNS name resolved on each tick. The resolver
	// uses the first IPv4 address returned to keep selection deterministic.
	EndpointHostname string `yaml:"endpoint_hostname"`

	// EndpointPort is the UDP port the WireGuard server listens on.
	EndpointPort int `yaml:"endpoint_port"`

	// ResolveInterval defines how often DNS is consulted.
	ResolveInterval time.Duration `yaml:"resolve_interval"`

	// StaleHandshakeThreshold forces an immediate endpoint update when the
	// most recent handshake is older than this even if the resolved IP did
	// not change. Catches "endpoint same, server moved hosts" cases.
	StaleHandshakeThreshold time.Duration `yaml:"stale_handshake_threshold"`
}

// WanDNSConfig configures the agent-side WAN-IP DDNS updater. The Cloudflare
// API token is read from the environment variable named in
// Cloudflare.APITokenEnv to keep secrets out of this struct.
type WanDNSConfig struct {
	// Enabled toggles the updater. When false, the updater does not run and
	// other fields are ignored.
	Enabled bool `yaml:"enabled"`

	// Hostname is the DNS record updated when the WAN IP changes.
	Hostname string `yaml:"hostname"`

	// Cloudflare identifies the API token env var and target zone.
	Cloudflare CloudflareConfig `yaml:"cloudflare"`

	// DetectionProviders lists URLs queried in order to discover the current
	// WAN IPv4 address. Tried sequentially; the first parseable response wins.
	DetectionProviders []string `yaml:"detection_providers"`

	// PollInterval defines how often the WAN IP is rechecked.
	PollInterval time.Duration `yaml:"poll_interval"`

	// Cooldown is the minimum time between successive Cloudflare record
	// updates. Prevents flapping during ISP DHCP renewal storms.
	Cooldown time.Duration `yaml:"cooldown"`
}

// CloudflareConfig identifies the Cloudflare API token env var and target zone
// used by the WAN DNS updater.
type CloudflareConfig struct {
	// APITokenEnv is the name of the environment variable that holds the
	// Cloudflare API token. The token must have DNS:Edit permission on the
	// configured zone.
	APITokenEnv string `yaml:"api_token_env"`

	// ZoneID is the Cloudflare zone identifier the record lives in.
	ZoneID string `yaml:"zone_id"`
}

// -------------------------------------------------------------------------
// LOADERS
// -------------------------------------------------------------------------

// LoadAgent reads agent-mode configuration. Requires the nodes list and OCI
// credentials. Wireguard and wan_dns blocks are validated only when enabled.
func LoadAgent(path string) (*Config, error) {
	cfg, err := parse(path, agentDefaults())
	if err != nil {
		return nil, err
	}
	if err := cfg.validateAgent(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

// LoadMonitor reads monitor-mode configuration. The config file is optional:
// when it does not exist, monitor runs with built-in defaults and no wireguard
// resolver, preserving the legacy env-only behaviour.
func LoadMonitor(path string) (*Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return monitorDefaults(), nil
	}
	cfg, err := parse(path, monitorDefaults())
	if err != nil {
		return nil, err
	}
	if err := cfg.validateMonitor(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return cfg, nil
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// parse reads the YAML file at path and unmarshals it into base. Defaults set
// on base are preserved for any fields the YAML file does not specify.
func parse(path string, base *Config) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, base); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return base, nil
}

// agentDefaults returns the default Config used as the base for LoadAgent.
func agentDefaults() *Config {
	return &Config{
		Timeout:       5 * time.Minute,
		CheckInterval: 30 * time.Second,
		ConsulAddress: "localhost:8500",
		OCI: OCIConfig{
			ConfigPath: "~/.oci/config",
			Profile:    "DEFAULT",
		},
		WanDNS: defaultWanDNS(),
	}
}

// monitorDefaults returns the default Config used as the base for LoadMonitor.
func monitorDefaults() *Config {
	return &Config{
		Wireguard: defaultWireguard(),
	}
}

// defaultWireguard returns the default WireguardConfig values applied when the
// YAML omits a field.
func defaultWireguard() WireguardConfig {
	return WireguardConfig{
		Interface:               "wg0",
		EndpointPort:            51820,
		ResolveInterval:         60 * time.Second,
		StaleHandshakeThreshold: 180 * time.Second,
	}
}

// defaultWanDNS returns the default WanDNSConfig values applied when the YAML
// omits a field.
func defaultWanDNS() WanDNSConfig {
	return WanDNSConfig{
		Cloudflare: CloudflareConfig{
			APITokenEnv: "CLOUDFLARE_API_TOKEN",
		},
		DetectionProviders: []string{
			"https://api.ipify.org",
			"https://1.1.1.1/cdn-cgi/trace",
		},
		PollInterval: 5 * time.Minute,
		Cooldown:     15 * time.Minute,
	}
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// validateAgent enforces the field requirements of agent mode plus any
// optional feature blocks the agent may enable.
func (c *Config) validateAgent() error {
	if len(c.Nodes) == 0 {
		return fmt.Errorf("no nodes configured")
	}
	for i, n := range c.Nodes {
		if n.Name == "" {
			return fmt.Errorf("node[%d]: missing name", i)
		}
		if n.InstanceID == "" {
			return fmt.Errorf("node[%d] %q: missing instance_id", i, n.Name)
		}
		if n.CompartmentID == "" {
			return fmt.Errorf("node[%d] %q: missing compartment_id", i, n.Name)
		}
	}
	if c.Timeout < time.Minute {
		return fmt.Errorf("timeout must be at least 1 minute")
	}
	if c.WanDNS.Enabled {
		if err := c.WanDNS.validate(); err != nil {
			return fmt.Errorf("wan_dns: %w", err)
		}
	}
	return nil
}

// validateMonitor enforces the field requirements of monitor mode. Only the
// optional wireguard block is checked; everything else is agent-mode-only.
func (c *Config) validateMonitor() error {
	if c.Wireguard.Enabled {
		if err := c.Wireguard.validate(); err != nil {
			return fmt.Errorf("wireguard: %w", err)
		}
	}
	return nil
}

// validate checks the WireguardConfig fields when the block is enabled.
func (w *WireguardConfig) validate() error {
	if w.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if w.PeerPubkey == "" {
		return fmt.Errorf("peer_pubkey is required")
	}
	if w.EndpointHostname == "" {
		return fmt.Errorf("endpoint_hostname is required")
	}
	if w.EndpointPort <= 0 || w.EndpointPort > 65535 {
		return fmt.Errorf("endpoint_port must be 1-65535")
	}
	if w.ResolveInterval < 10*time.Second {
		return fmt.Errorf("resolve_interval must be at least 10s")
	}
	if w.StaleHandshakeThreshold < 30*time.Second {
		return fmt.Errorf("stale_handshake_threshold must be at least 30s")
	}
	return nil
}

// validate checks the WanDNSConfig fields when the block is enabled.
func (w *WanDNSConfig) validate() error {
	if w.Hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	if w.Cloudflare.APITokenEnv == "" {
		return fmt.Errorf("cloudflare.api_token_env is required")
	}
	if w.Cloudflare.ZoneID == "" {
		return fmt.Errorf("cloudflare.zone_id is required")
	}
	if len(w.DetectionProviders) == 0 {
		return fmt.Errorf("detection_providers must list at least one URL")
	}
	if w.PollInterval < 30*time.Second {
		return fmt.Errorf("poll_interval must be at least 30s")
	}
	if w.Cooldown < time.Minute {
		return fmt.Errorf("cooldown must be at least 1m")
	}
	return nil
}
