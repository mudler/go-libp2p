package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/event"
	"github.com/libp2p/go-libp2p-core/peer"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	quic "github.com/libp2p/go-libp2p-quic-transport"
	"github.com/libp2p/go-tcp-transport"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

var (
	relayServers  = []peer.AddrInfo{}
	privateKeyHex = "08011240b5e6951900c9fb878b75a38b35d51d1279843df1e54b1d432a765882ed17f03bc92f4027a85925927042e5a85c2ac3fc9585a88cb1db612253850093be957d2b"
)

func main() {
	var test_mode string
	flag.StringVar(&test_mode, "test_mode", "quic", "Test TCP or QUIC hole punching")
	flag.Parse()
	if test_mode != "tcp" && test_mode != "quic" {
		panic(errors.New("test mode should be tcp or quic"))
	}
	fmt.Println("\n test server initiated in mode:", test_mode)

	skbz, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		panic(err)
	}
	sk, err := crypto.UnmarshalPrivateKey(skbz)
	if err != nil {
		panic(err)
	}

	// transports and addresses
	var transportOpts []libp2p.Option
	if test_mode == "tcp" {
		transportOpts = append(transportOpts, libp2p.Transport(tcp.NewTCPTransport), libp2p.ListenAddrs(ma.StringCast("/ip4/0.0.0.0/tcp/12345")))
	} else {
		transportOpts = append(transportOpts, libp2p.Transport(quic.NewTransport), libp2p.ListenAddrs(ma.StringCast("/ip4/0.0.0.0/udp/12345/quic")))
	}

	// create host with hole punching enabled. we also enable AutorRelay with static servers so peer can connect to and
	// advertise relay addresses on it's own.
	ctx := context.Background()
	h1, err := libp2p.New(ctx,
		libp2p.Identity(sk),
		libp2p.EnableHolePunching(),
		libp2p.EnableAutoRelay(),
		libp2p.ForceReachabilityPrivate(),
		libp2p.StaticRelays(relayServers),
		transportOpts[0],
		transportOpts[1],
	)
	if err != nil {
		panic(err)
	}
	// subscribe for address change event so we can detect when we discover an observed public non proxy address
	sub, err := h1.EventBus().Subscribe(new(event.EvtLocalAddressesUpdated))
	if err != nil {
		panic(err)
	}

	// bootstrap with dht so we have some connections and activated observed addresses and our address gets advertised to the world.
	d, err := dht.New(ctx, h1, dht.Mode(dht.ModeClient), dht.BootstrapPeers(dht.GetDefaultBootstrapPeerAddrInfos()...))
	if err != nil {
		panic(err)
	}
	// block till we have an observed public address.
LOOP:
	for {
		select {
		case ev := <-sub.Out():
			aev := ev.(event.EvtLocalAddressesUpdated)
			for _, a := range aev.Current {
				_, err := a.Address.ValueForProtocol(ma.P_CIRCUIT)
				if manet.IsPublicAddr(a.Address) && err != nil {
					break LOOP
				}
			}
		case <-time.After(10 * time.Second):
			panic(errors.New("did not get public address"))
		}
	}
	fmt.Println("\n Peer has discovered public addresses for self")

	// one more round of refresh so our observed address also gets propogated to the network.
	<-d.ForceRefresh()

	fmt.Printf("\n Server peer is up and ready for hole punching, peer ID is %s", h1.ID())
	fmt.Println("\n peer address are:")
	for _, a := range h1.Addrs() {
		fmt.Println("\n ", a)
	}
	select {
	case <-time.After(1 * time.Hour):
	}
}