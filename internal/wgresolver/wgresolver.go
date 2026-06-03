// -------------------------------------------------------------------------------
// Oracle Watchdog - WireGuard Endpoint Resolver
//
// Author: Alex Freidah
//
// Periodically re-resolves the configured WireGuard server hostname and updates
// the kernel peer endpoint via netlink when the resolved IP changes. Also
// triggers an immediate re-resolve when the most recent peer handshake is
// older than the configured stale threshold, which catches cases where the
// hostname still resolves to the same IP but the server has moved hosts.
//
// Self-healing design: never crashes due to DNS or netlink unavailability.
// Continuously retries on the next tick and emits metrics on current state.
// -------------------------------------------------------------------------------

package wgresolver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"go.opentelemetry.io/otel/codes"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/afreidah/oracle-watchdog/internal/config"
	"github.com/afreidah/oracle-watchdog/internal/metrics"
	"github.com/afreidah/oracle-watchdog/internal/tracing"
)

// -------------------------------------------------------------------------
// INTERFACES
// -------------------------------------------------------------------------

// WGClient narrows the wgctrl surface so tests can substitute fakes without
// requiring a real WireGuard interface or root privileges.
type WGClient interface {
	// Device returns the WireGuard device state for the named interface.
	Device(name string) (*wgtypes.Device, error)

	// ConfigureDevice applies the supplied configuration to the named
	// interface. The endpoint resolver only sets per-peer Endpoint fields
	// with UpdateOnly=true.
	ConfigureDevice(name string, cfg wgtypes.Config) error

	// Close releases the underlying netlink socket.
	Close() error
}

// Resolver narrows the DNS surface so tests can substitute fakes without
// hitting real DNS infrastructure.
type Resolver interface {
	// LookupIP behaves like net.Resolver.LookupIP. The endpoint resolver
	// always passes "ip4" for the network argument.
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// -------------------------------------------------------------------------
// SENTINEL ERRORS
// -------------------------------------------------------------------------

// ErrPeerNotFound is returned when the configured peer pubkey is absent from
// the WireGuard device's peer list.
var ErrPeerNotFound = errors.New("peer not found on interface")

// ErrNoIPv4 is returned when DNS resolution returns no IPv4 addresses for the
// configured endpoint hostname.
var ErrNoIPv4 = errors.New("no IPv4 address resolved for endpoint")

// -------------------------------------------------------------------------
// ENDPOINT RESOLVER
// -------------------------------------------------------------------------

// EndpointResolver maintains a fresh peer endpoint by polling DNS and updating
// the kernel WireGuard configuration when the resolved IP drifts.
type EndpointResolver struct {
	// cfg holds the validated configuration the resolver was constructed with.
	cfg config.WireguardConfig

	// wg is the netlink client used to read and update the WireGuard device.
	wg WGClient

	// dns resolves the endpoint hostname on each tick.
	dns Resolver

	// pubkey is the parsed wgtypes.Key form of cfg.PeerPubkey, computed once
	// at construction so each tick avoids re-parsing.
	pubkey wgtypes.Key

	// log is the component-scoped logger used by all resolver output.
	log *slog.Logger

	// ownsClient records whether wg was created internally (and therefore
	// must be closed by Close) or injected by a caller via WithWGClient.
	ownsClient bool

	// mu guards lastIP from concurrent access by Run and external callers
	// reading metrics.
	mu sync.Mutex

	// lastIP is the most recently observed endpoint IP, tracked so the
	// previous label value can be deleted from the WgEndpointCurrentIP
	// gauge when it changes.
	lastIP string
}

// Option configures the EndpointResolver. Used to inject test doubles for the
// wgctrl client and DNS resolver.
type Option func(*EndpointResolver)

// WithWGClient overrides the default wgctrl client. The supplied client's
// Close method is not called by the resolver.
func WithWGClient(c WGClient) Option {
	return func(r *EndpointResolver) {
		r.wg = c
		r.ownsClient = false
	}
}

// WithResolver overrides the default DNS resolver.
func WithResolver(d Resolver) Option {
	return func(r *EndpointResolver) { r.dns = d }
}

// WithLogger overrides the default scoped logger.
func WithLogger(l *slog.Logger) Option {
	return func(r *EndpointResolver) { r.log = l }
}

// New constructs a resolver from the given config. The default wgctrl client
// is opened immediately so configuration errors surface at construction time;
// the default DNS resolver is the Go stdlib resolver.
func New(cfg config.WireguardConfig, opts ...Option) (*EndpointResolver, error) {
	pubkey, err := wgtypes.ParseKey(cfg.PeerPubkey)
	if err != nil {
		return nil, fmt.Errorf("parse peer_pubkey: %w", err)
	}

	r := &EndpointResolver{
		cfg:    cfg,
		pubkey: pubkey,
		dns:    &net.Resolver{},
		log:    slog.Default().With("component", "wgresolver"),
	}

	for _, opt := range opts {
		opt(r)
	}

	if r.wg == nil {
		client, err := wgctrl.New()
		if err != nil {
			return nil, fmt.Errorf("open wgctrl: %w", err)
		}
		r.wg = client
		r.ownsClient = true
	}

	return r, nil
}

// Close releases resources held by the default wgctrl client. Safe to call when
// a custom client was injected via WithWGClient (no-op in that case).
func (r *EndpointResolver) Close() error {
	if r.ownsClient && r.wg != nil {
		return r.wg.Close()
	}
	return nil
}

// -------------------------------------------------------------------------
// RUN LOOP
// -------------------------------------------------------------------------

// Run drives the resolve loop until ctx is cancelled. Performs an initial
// tick on entry so failover-driven endpoint changes are picked up promptly
// after the resolver starts.
func (r *EndpointResolver) Run(ctx context.Context) {
	r.log.InfoContext(ctx, "starting wireguard endpoint resolver",
		"interface", r.cfg.Interface,
		"endpoint", fmt.Sprintf("%s:%d", r.cfg.EndpointHostname, r.cfg.EndpointPort),
		"resolve_interval", r.cfg.ResolveInterval,
	)

	r.tick(ctx)

	ticker := time.NewTicker(r.cfg.ResolveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.InfoContext(ctx, "stopping wireguard endpoint resolver")
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// -------------------------------------------------------------------------
// TICK
// -------------------------------------------------------------------------

// tick performs one resolve-compare-update cycle. Reads the device state,
// resolves the endpoint hostname, and updates the peer endpoint when either
// the resolved IP differs from the current one or the last handshake is
// older than the stale threshold.
func (r *EndpointResolver) tick(ctx context.Context) {
	ctx, span := tracing.StartClientSpan(ctx, "wgresolver.tick",
		tracing.PeerServiceAttr("wireguard"),
	)
	defer span.End()

	device, err := r.wg.Device(r.cfg.Interface)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "device lookup failed")
		r.log.WarnContext(ctx, "wireguard device lookup failed",
			"interface", r.cfg.Interface,
			"error", err,
		)
		metrics.WgEndpointResolutionFailures.Inc()
		return
	}

	peer, ok := findPeer(device.Peers, r.pubkey)
	if !ok {
		span.SetStatus(codes.Error, ErrPeerNotFound.Error())
		r.log.WarnContext(ctx, "configured peer pubkey missing from device",
			"interface", r.cfg.Interface,
			"pubkey", r.pubkey.String(),
		)
		metrics.WgEndpointResolutionFailures.Inc()
		return
	}

	r.recordHandshakeAge(peer)
	stale := isStaleHandshake(peer.LastHandshakeTime, r.cfg.StaleHandshakeThreshold)

	resolved, err := r.resolve(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "dns resolution failed")
		r.log.WarnContext(ctx, "endpoint dns resolution failed",
			"hostname", r.cfg.EndpointHostname,
			"error", err,
		)
		metrics.WgEndpointResolutionFailures.Inc()
		return
	}

	current := endpointIP(peer.Endpoint)
	if !stale && current == resolved.String() {
		r.recordCurrentIP(current)
		span.SetStatus(codes.Ok, "no change")
		return
	}

	if err := r.applyEndpoint(resolved); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "configure device failed")
		r.log.WarnContext(ctx, "wireguard endpoint update failed",
			"interface", r.cfg.Interface,
			"resolved_ip", resolved.String(),
			"error", err,
		)
		metrics.WgEndpointResolutionFailures.Inc()
		return
	}

	r.log.InfoContext(ctx, "wireguard peer endpoint updated",
		"interface", r.cfg.Interface,
		"previous_ip", current,
		"new_ip", resolved.String(),
		"stale_handshake", stale,
	)
	metrics.WgEndpointChanges.Inc()
	metrics.WgEndpointLastUpdate.SetToCurrentTime()
	r.recordCurrentIP(resolved.String())
	span.SetStatus(codes.Ok, "endpoint updated")
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// resolve performs an IPv4 lookup of the configured endpoint hostname and
// returns the first address. Deterministic selection keeps the chosen
// endpoint stable when DNS round-robins multiple A records.
func (r *EndpointResolver) resolve(ctx context.Context) (net.IP, error) {
	ips, err := r.dns.LookupIP(ctx, "ip4", r.cfg.EndpointHostname)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, ErrNoIPv4
	}
	return ips[0], nil
}

// applyEndpoint sets the peer endpoint to the supplied IP and configured port.
// UpdateOnly=true ensures the call leaves all other peer fields (allowed-ips,
// preshared key, keepalive) untouched.
func (r *EndpointResolver) applyEndpoint(ip net.IP) error {
	endpoint := &net.UDPAddr{IP: ip, Port: r.cfg.EndpointPort}
	cfg := wgtypes.Config{
		Peers: []wgtypes.PeerConfig{{
			PublicKey:  r.pubkey,
			UpdateOnly: true,
			Endpoint:   endpoint,
		}},
	}
	return r.wg.ConfigureDevice(r.cfg.Interface, cfg)
}

// recordCurrentIP updates the WgEndpointCurrentIP gauge so dashboards see the
// current resolved IP. Deletes the previous label value to avoid accumulating
// stale time series after every IP change.
func (r *EndpointResolver) recordCurrentIP(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastIP == ip {
		return
	}
	if r.lastIP != "" {
		metrics.WgEndpointCurrentIP.DeleteLabelValues(r.cfg.Interface, r.pubkey.String(), r.lastIP)
	}
	r.lastIP = ip
	metrics.WgEndpointCurrentIP.WithLabelValues(r.cfg.Interface, r.pubkey.String(), ip).Set(1)
}

// recordHandshakeAge updates the WgPeerHandshakeAge gauge with the seconds
// since the last successful handshake. Reports -1 when no handshake has ever
// completed so dashboards can distinguish "never" from "very old".
func (r *EndpointResolver) recordHandshakeAge(peer wgtypes.Peer) {
	if peer.LastHandshakeTime.IsZero() {
		metrics.WgPeerHandshakeAge.WithLabelValues(r.pubkey.String()).Set(-1)
		return
	}
	age := time.Since(peer.LastHandshakeTime).Seconds()
	metrics.WgPeerHandshakeAge.WithLabelValues(r.pubkey.String()).Set(age)
}

// findPeer returns the first peer in peers whose public key matches target.
// Iterates by index because each Peer struct is large and copying it on every
// loop iteration is wasteful.
func findPeer(peers []wgtypes.Peer, target wgtypes.Key) (wgtypes.Peer, bool) {
	for i := range peers {
		if peers[i].PublicKey == target {
			return peers[i], true
		}
	}
	return wgtypes.Peer{}, false
}

// endpointIP returns the IP portion of a UDPAddr as a string, or empty string
// when the address or its IP is nil.
func endpointIP(addr *net.UDPAddr) string {
	if addr == nil || addr.IP == nil {
		return ""
	}
	return addr.IP.String()
}

// isStaleHandshake reports whether last is non-zero and older than threshold.
// A zero handshake time is treated as not stale because the peer simply has
// not handshaken yet (typical right after wg-quick up).
func isStaleHandshake(last time.Time, threshold time.Duration) bool {
	if last.IsZero() {
		return false
	}
	return time.Since(last) > threshold
}
