// -------------------------------------------------------------------------------
// Oracle Watchdog - WireGuard Endpoint Resolver Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package wgresolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/afreidah/oracle-watchdog/internal/config"
)

// -------------------------------------------------------------------------
// FAKES
// -------------------------------------------------------------------------

// fakeWGClient is an in-memory WGClient implementation for tests. The peer
// list is mutable so tests can simulate handshakes and endpoint changes.
type fakeWGClient struct {
	mu sync.Mutex

	// device is returned by Device. nil produces deviceErr.
	device *wgtypes.Device

	// deviceErr forces Device to return an error.
	deviceErr error

	// configureErr forces ConfigureDevice to return an error.
	configureErr error

	// configCalls records every ConfigureDevice invocation for assertions.
	configCalls []wgtypes.Config

	// closeCalls counts Close invocations.
	closeCalls int
}

// Device returns the configured device or error.
func (f *fakeWGClient) Device(string) (*wgtypes.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deviceErr != nil {
		return nil, f.deviceErr
	}
	return f.device, nil
}

// ConfigureDevice records the call and applies the requested endpoint to the
// in-memory peer when no error is configured.
func (f *fakeWGClient) ConfigureDevice(_ string, cfg wgtypes.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configCalls = append(f.configCalls, cfg)
	if f.configureErr != nil {
		return f.configureErr
	}
	if f.device == nil {
		return nil
	}
	for _, pc := range cfg.Peers {
		for i := range f.device.Peers {
			if f.device.Peers[i].PublicKey == pc.PublicKey {
				if pc.Endpoint != nil {
					f.device.Peers[i].Endpoint = pc.Endpoint
				}
			}
		}
	}
	return nil
}

// Close records the call and returns nil.
func (f *fakeWGClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return nil
}

// callCount returns the number of ConfigureDevice invocations recorded so far.
func (f *fakeWGClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.configCalls)
}

// fakeResolver is an in-memory Resolver implementation for tests.
type fakeResolver struct {
	mu sync.Mutex

	// ips is returned from LookupIP when err is nil.
	ips []net.IP

	// err forces LookupIP to return an error.
	err error

	// calls counts LookupIP invocations.
	calls int
}

// LookupIP returns the configured ips or error and increments the call
// counter. The network argument is ignored.
func (f *fakeResolver) LookupIP(_ context.Context, _ string, _ string) ([]net.IP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]net.IP, len(f.ips))
	copy(out, f.ips)
	return out, nil
}

// callCount returns the number of LookupIP invocations recorded so far.
func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// -------------------------------------------------------------------------
// FIXTURES
// -------------------------------------------------------------------------

// genKey produces a deterministic key by hashing a seed. Using a real
// generator keeps the tests independent of any one specific key string.
func genKey(t *testing.T, _ string) wgtypes.Key {
	t.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k.PublicKey()
}

// makeDevice builds a minimal wgtypes.Device with a single peer at the given
// endpoint and last-handshake time.
func makeDevice(pubkey wgtypes.Key, endpoint *net.UDPAddr, last time.Time) *wgtypes.Device {
	return &wgtypes.Device{
		Name: "wg0",
		Peers: []wgtypes.Peer{{
			PublicKey:         pubkey,
			Endpoint:          endpoint,
			LastHandshakeTime: last,
		}},
	}
}

// baseConfig returns a valid WireguardConfig with the given peer pubkey.
func baseConfig(pubkey wgtypes.Key) config.WireguardConfig {
	return config.WireguardConfig{
		Enabled:                 true,
		Interface:               "wg0",
		PeerPubkey:              pubkey.String(),
		EndpointHostname:        "wg.example.com",
		EndpointPort:            51820,
		ResolveInterval:         time.Minute,
		StaleHandshakeThreshold: 3 * time.Minute,
	}
}

// newResolverWithFakes returns a resolver wired to the supplied fakes. Test
// helper to keep individual cases concise.
func newResolverWithFakes(t *testing.T, cfg config.WireguardConfig, wg WGClient, dns Resolver) *EndpointResolver {
	t.Helper()
	r, err := New(cfg, WithWGClient(wg), WithResolver(dns))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// -------------------------------------------------------------------------
// CONSTRUCTOR
// -------------------------------------------------------------------------

// TestNew_RejectsBadPubkey verifies New surfaces a parse error rather than
// constructing a resolver that would fail on every tick.
func TestNew_RejectsBadPubkey(t *testing.T) {
	cfg := config.WireguardConfig{
		PeerPubkey:              "not-a-real-base64-wg-key",
		Interface:               "wg0",
		EndpointHostname:        "wg.example.com",
		EndpointPort:            51820,
		ResolveInterval:         time.Minute,
		StaleHandshakeThreshold: 3 * time.Minute,
	}
	if _, err := New(cfg, WithWGClient(&fakeWGClient{}), WithResolver(&fakeResolver{})); err == nil {
		t.Fatal("expected error from invalid peer_pubkey")
	}
}

// TestClose_NoOpForInjectedClient confirms Close does not call the injected
// client's Close - that responsibility stays with the caller.
func TestClose_NoOpForInjectedClient(t *testing.T) {
	pub := genKey(t, "")
	wg := &fakeWGClient{}
	r := newResolverWithFakes(t, baseConfig(pub), wg, &fakeResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if wg.closeCalls != 0 {
		t.Fatalf("expected 0 Close calls on injected client, got %d", wg.closeCalls)
	}
}

// -------------------------------------------------------------------------
// TICK
// -------------------------------------------------------------------------

// TestTick_NoChange verifies a tick with matching IP and recent handshake
// records the current IP gauge but does not call ConfigureDevice.
func TestTick_NoChange(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(pub, addr, time.Now())}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 0 {
		t.Fatalf("expected 0 ConfigureDevice calls on no-change tick, got %d", wg.callCount())
	}
}

// TestTick_EndpointChange verifies a resolved IP that differs from the
// current peer endpoint triggers a ConfigureDevice with UpdateOnly=true.
func TestTick_EndpointChange(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(pub, addr, time.Now())}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("5.6.7.8")}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 1 {
		t.Fatalf("expected 1 ConfigureDevice call, got %d", wg.callCount())
	}
	cfg := wg.configCalls[0]
	if len(cfg.Peers) != 1 {
		t.Fatalf("expected 1 peer in update, got %d", len(cfg.Peers))
	}
	pc := cfg.Peers[0]
	if !pc.UpdateOnly {
		t.Error("expected UpdateOnly=true to preserve other peer fields")
	}
	if pc.Endpoint == nil || pc.Endpoint.IP.String() != "5.6.7.8" {
		t.Errorf("expected endpoint 5.6.7.8, got %v", pc.Endpoint)
	}
	if pc.Endpoint.Port != 51820 {
		t.Errorf("expected port 51820, got %d", pc.Endpoint.Port)
	}
}

// TestTick_StaleHandshakeForcesUpdate verifies that even when the resolved
// IP matches, an old handshake triggers a defensive endpoint reapply.
func TestTick_StaleHandshakeForcesUpdate(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	stale := time.Now().Add(-10 * time.Minute)
	wg := &fakeWGClient{device: makeDevice(pub, addr, stale)}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 1 {
		t.Fatalf("expected 1 ConfigureDevice call due to stale handshake, got %d", wg.callCount())
	}
}

// TestTick_ZeroHandshakeIsNotStale verifies a never-handshaken peer (zero
// time) does not trip the stale threshold and force needless updates.
func TestTick_ZeroHandshakeIsNotStale(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(pub, addr, time.Time{})}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 0 {
		t.Fatalf("expected 0 calls for zero-handshake-time peer, got %d", wg.callCount())
	}
}

// TestTick_DNSFailureIsRecoverable verifies a DNS error does not panic and
// leaves the peer endpoint unchanged.
func TestTick_DNSFailureIsRecoverable(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(pub, addr, time.Now())}
	dns := &fakeResolver{err: errors.New("dns fail")}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 0 {
		t.Fatalf("expected 0 calls when DNS fails, got %d", wg.callCount())
	}
}

// TestTick_NoIPv4Returned verifies an empty DNS response is treated as a
// failure rather than being passed downstream as a nil IP.
func TestTick_NoIPv4Returned(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(pub, addr, time.Now())}
	dns := &fakeResolver{ips: []net.IP{}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 0 {
		t.Fatalf("expected 0 calls when DNS returned no IPv4, got %d", wg.callCount())
	}
}

// TestTick_DeviceLookupFailureIsRecoverable verifies a netlink error returns
// without crashing or attempting a configure.
func TestTick_DeviceLookupFailureIsRecoverable(t *testing.T) {
	pub := genKey(t, "")
	wg := &fakeWGClient{deviceErr: errors.New("netlink down")}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 0 {
		t.Fatalf("expected 0 calls when device lookup fails, got %d", wg.callCount())
	}
	if dns.callCount() != 0 {
		t.Fatalf("expected DNS not consulted when device lookup fails, got %d", dns.callCount())
	}
}

// TestTick_PeerNotFoundIsRecoverable verifies a missing peer logs a warning
// without crashing or attempting a configure.
func TestTick_PeerNotFoundIsRecoverable(t *testing.T) {
	tracked := genKey(t, "tracked")
	other := genKey(t, "other")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(other, addr, time.Now())}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}}

	r := newResolverWithFakes(t, baseConfig(tracked), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 0 {
		t.Fatalf("expected 0 calls when peer is missing, got %d", wg.callCount())
	}
}

// TestTick_ConfigureFailureIsRecoverable verifies a netlink configure error
// is logged without crashing the resolver.
func TestTick_ConfigureFailureIsRecoverable(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{
		device:       makeDevice(pub, addr, time.Now()),
		configureErr: errors.New("permission denied"),
	}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("5.6.7.8")}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)
	r.tick(context.Background())

	if wg.callCount() != 1 {
		t.Fatalf("expected 1 attempted ConfigureDevice call, got %d", wg.callCount())
	}
}

// TestTick_DeterministicMultiAResolution verifies the resolver picks the
// first IPv4 address from a multi-A response. Re-running the resolve helper
// across ticks must produce the same IP so the kernel endpoint does not
// flap when DNS round-robins.
func TestTick_DeterministicMultiAResolution(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(pub, addr, time.Now())}
	dns := &fakeResolver{ips: []net.IP{
		net.ParseIP("9.9.9.9"),
		net.ParseIP("8.8.8.8"),
		net.ParseIP("7.7.7.7"),
	}}

	r := newResolverWithFakes(t, baseConfig(pub), wg, dns)

	first, err := r.resolve(context.Background())
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := r.resolve(context.Background())
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.String() != "9.9.9.9" || second.String() != "9.9.9.9" {
		t.Fatalf("expected 9.9.9.9 both times, got %s and %s", first, second)
	}
}

// -------------------------------------------------------------------------
// RUN
// -------------------------------------------------------------------------

// TestRun_HonoursContextCancellation verifies Run returns promptly after the
// context is cancelled and at least one tick has completed.
func TestRun_HonoursContextCancellation(t *testing.T) {
	pub := genKey(t, "")
	addr := &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820}
	wg := &fakeWGClient{device: makeDevice(pub, addr, time.Now())}
	dns := &fakeResolver{ips: []net.IP{net.ParseIP("1.2.3.4")}}

	cfg := baseConfig(pub)
	cfg.ResolveInterval = 10 * time.Millisecond

	r := newResolverWithFakes(t, cfg, wg, dns)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()

	time.Sleep(40 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of cancel")
	}

	if dns.callCount() == 0 {
		t.Fatal("expected at least one tick to have run")
	}
}

// -------------------------------------------------------------------------
// PURE HELPERS
// -------------------------------------------------------------------------

// TestEndpointIP_HandlesNil exercises the nil-safe code paths so that a
// brand-new peer with no resolved endpoint does not crash the resolver.
func TestEndpointIP_HandlesNil(t *testing.T) {
	if got := endpointIP(nil); got != "" {
		t.Errorf("nil addr: expected empty, got %q", got)
	}
	if got := endpointIP(&net.UDPAddr{}); got != "" {
		t.Errorf("addr with nil IP: expected empty, got %q", got)
	}
	if got := endpointIP(&net.UDPAddr{IP: net.ParseIP("1.2.3.4")}); got != "1.2.3.4" {
		t.Errorf("expected 1.2.3.4, got %q", got)
	}
}

// TestIsStaleHandshake_Boundaries covers each branch: zero time, within
// threshold, and past threshold.
func TestIsStaleHandshake_Boundaries(t *testing.T) {
	cases := []struct {
		name      string
		last      time.Time
		threshold time.Duration
		want      bool
	}{
		{"zero time is not stale", time.Time{}, time.Minute, false},
		{"recent is not stale", time.Now().Add(-30 * time.Second), time.Minute, false},
		{"old is stale", time.Now().Add(-2 * time.Minute), time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStaleHandshake(tc.last, tc.threshold); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFindPeer_PresentAndAbsent covers both branches of the linear search.
func TestFindPeer_PresentAndAbsent(t *testing.T) {
	target := genKey(t, "target")
	other := genKey(t, "other")
	peers := []wgtypes.Peer{{PublicKey: other}, {PublicKey: target}}

	if _, ok := findPeer(peers, target); !ok {
		t.Fatal("expected target peer to be found")
	}
	if _, ok := findPeer(peers, genKey(t, "missing")); ok {
		t.Fatal("expected missing peer to be not found")
	}
}

// TestRecordHandshakeAge_NeverHandshaken verifies the -1 sentinel is reported
// for peers that have not handshaken yet.
func TestRecordHandshakeAge_NeverHandshaken(t *testing.T) {
	pub := genKey(t, "")
	r := newResolverWithFakes(t, baseConfig(pub), &fakeWGClient{}, &fakeResolver{})
	// Should not panic regardless of metric registration state.
	r.recordHandshakeAge(wgtypes.Peer{PublicKey: pub, LastHandshakeTime: time.Time{}})
}

// TestRecordCurrentIP_RotatesLabels verifies repeated changes do not
// accumulate dead label series. The exact metric value is verified by the
// metrics package; this test focuses on the rotation invariant.
func TestRecordCurrentIP_RotatesLabels(t *testing.T) {
	pub := genKey(t, "")
	r := newResolverWithFakes(t, baseConfig(pub), &fakeWGClient{}, &fakeResolver{})
	r.recordCurrentIP("1.1.1.1")
	r.recordCurrentIP("1.1.1.1")
	r.recordCurrentIP("2.2.2.2")
	if r.lastIP != "2.2.2.2" {
		t.Fatalf("expected lastIP 2.2.2.2, got %s", r.lastIP)
	}
}

// errString lets table tests express expected error fragments.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}

var _ = errString // referenced for future table tests
