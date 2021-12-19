package autorelay_test

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	relayv1 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv1/relay"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"

	"github.com/libp2p/go-libp2p-core/event"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"

	discovery "github.com/libp2p/go-libp2p-discovery"

	"github.com/ipfs/go-cid"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
	"github.com/stretchr/testify/require"
)

// test specific parameters
func init() {
	autorelay.BootDelay = 1 * time.Second
	autorelay.AdvertiseBootDelay = 100 * time.Millisecond
}

// mock routing
type mockRoutingTable struct {
	mx        sync.Mutex
	providers map[string]map[peer.ID]peer.AddrInfo
	peers     map[peer.ID]peer.AddrInfo
}

type mockRouting struct {
	h   host.Host
	tab *mockRoutingTable
}

func newMockRoutingTable() *mockRoutingTable {
	return &mockRoutingTable{providers: make(map[string]map[peer.ID]peer.AddrInfo)}
}

func newMockRouting(h host.Host, tab *mockRoutingTable) *mockRouting {
	return &mockRouting{h: h, tab: tab}
}

func (m *mockRouting) FindPeer(ctx context.Context, p peer.ID) (peer.AddrInfo, error) {
	m.tab.mx.Lock()
	defer m.tab.mx.Unlock()
	pi, ok := m.tab.peers[p]
	if !ok {
		return peer.AddrInfo{}, routing.ErrNotFound
	}
	return pi, nil
}

func (m *mockRouting) Provide(ctx context.Context, cid cid.Cid, bcast bool) error {
	m.tab.mx.Lock()
	defer m.tab.mx.Unlock()

	pmap, ok := m.tab.providers[cid.String()]
	if !ok {
		pmap = make(map[peer.ID]peer.AddrInfo)
		m.tab.providers[cid.String()] = pmap
	}

	pi := peer.AddrInfo{ID: m.h.ID(), Addrs: m.h.Addrs()}
	pmap[m.h.ID()] = pi
	if m.tab.peers == nil {
		m.tab.peers = make(map[peer.ID]peer.AddrInfo)
	}
	m.tab.peers[m.h.ID()] = pi

	return nil
}

func (m *mockRouting) FindProvidersAsync(ctx context.Context, cid cid.Cid, limit int) <-chan peer.AddrInfo {
	ch := make(chan peer.AddrInfo)
	go func() {
		defer close(ch)
		m.tab.mx.Lock()
		defer m.tab.mx.Unlock()

		pmap, ok := m.tab.providers[cid.String()]
		if !ok {
			return
		}

		for _, pi := range pmap {
			select {
			case ch <- pi:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch
}

func connect(t *testing.T, a, b host.Host) {
	pinfo := peer.AddrInfo{ID: a.ID(), Addrs: a.Addrs()}
	require.NoError(t, b.Connect(context.Background(), pinfo))
}

// and the actual test!
func TestAutoRelay(t *testing.T) {
	private4 := manet.Private4
	t.Cleanup(func() { manet.Private4 = private4 })
	manet.Private4 = []*net.IPNet{}

	t.Run("with a circuitv1 relay", func(t *testing.T) {
		testAutoRelay(t, false)
	})
	t.Run("testing autorelay with circuitv2 relay", func(t *testing.T) {
		testAutoRelay(t, true)
	})
}

func isRelayAddr(addr ma.Multiaddr) bool {
	_, err := addr.ValueForProtocol(ma.P_CIRCUIT)
	return err == nil
}

func testAutoRelay(t *testing.T, useRelayv2 bool) {
	mtab := newMockRoutingTable()
	makeRouting := func(h host.Host) (*mockRouting, error) {
		mr := newMockRouting(h, mtab)
		return mr, nil
	}
	makePeerRouting := func(h host.Host) (routing.PeerRouting, error) {
		return makeRouting(h)
	}

	// this is the relay host
	// announce dns addrs because filter out private addresses from relays,
	// and we consider dns addresses "public".
	relayHost, err := libp2p.New(
		libp2p.DisableRelay(),
		libp2p.AddrsFactory(func(addrs []ma.Multiaddr) []ma.Multiaddr {
			for i, addr := range addrs {
				saddr := addr.String()
				if strings.HasPrefix(saddr, "/ip4/127.0.0.1/") {
					addrNoIP := strings.TrimPrefix(saddr, "/ip4/127.0.0.1")
					addrs[i] = ma.StringCast("/dns4/localhost" + addrNoIP)
				}
			}
			return addrs
		}))
	require.NoError(t, err)
	defer relayHost.Close()

	// instantiate the relay
	if useRelayv2 {
		r, err := relayv2.New(relayHost)
		require.NoError(t, err)
		defer r.Close()
	} else {
		r, err := relayv1.NewRelay(relayHost)
		require.NoError(t, err)
		defer r.Close()
	}

	// advertise the relay
	relayRouting, err := makeRouting(relayHost)
	require.NoError(t, err)
	relayDiscovery := discovery.NewRoutingDiscovery(relayRouting)
	autorelay.Advertise(context.Background(), relayDiscovery)

	// the client hosts
	h1, err := libp2p.New(libp2p.EnableRelay())
	require.NoError(t, err)
	defer h1.Close()

	h2, err := libp2p.New(libp2p.EnableRelay(), libp2p.EnableAutoRelay(), libp2p.Routing(makePeerRouting))
	require.NoError(t, err)
	defer h2.Close()

	// verify that we don't advertise relay addrs initially
	for _, addr := range h2.Addrs() {
		if isRelayAddr(addr) {
			t.Fatal("relay addr advertised before auto detection")
		}
	}

	// connect to AutoNAT, have it resolve to private.
	connect(t, h1, h2)

	privEmitter, _ := h2.EventBus().Emitter(new(event.EvtLocalReachabilityChanged))
	privEmitter.Emit(event.EvtLocalReachabilityChanged{Reachability: network.ReachabilityPrivate})

	hasRelayAddrs := func(t *testing.T, addrs []ma.Multiaddr) bool {
		unspecificRelay := ma.StringCast("/p2p-circuit")
		for _, addr := range addrs {
			if addr.Equal(unspecificRelay) {
				t.Fatal("unspecific relay addr advertised")
			}
			if isRelayAddr(addr) {
				return true
			}
		}
		return false
	}
	// Wait for detection to do its magic
	require.Eventually(t, func() bool { return hasRelayAddrs(t, h2.Addrs()) }, 3*time.Second, 30*time.Millisecond)

	// verify that we now advertise relay addrs (but not unspecific relay addrs)
	if !hasRelayAddrs(t, h2.Addrs()) {
		t.Fatal("No relay addrs advertised")
	}

	// verify that we can connect through the relay
	var raddrs []ma.Multiaddr
	for _, addr := range h2.Addrs() {
		if isRelayAddr(addr) {
			raddrs = append(raddrs, addr)
		}
	}
	h3, err := libp2p.New(libp2p.EnableRelay())
	require.NoError(t, err)
	defer h3.Close()
	require.NoError(t, h3.Connect(context.Background(), peer.AddrInfo{ID: h2.ID(), Addrs: raddrs}))

	// verify that we have pushed relay addrs to connected peers
	if !hasRelayAddrs(t, h1.Peerstore().Addrs(h2.ID())) {
		t.Fatal("No relay addrs pushed")
	}
}
