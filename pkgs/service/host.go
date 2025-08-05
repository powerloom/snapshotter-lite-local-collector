package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/routing"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	"github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
)

// NewHost creates a new libp2p host and connects to bootstrap peers.
func NewHost(ctx context.Context, bootstrapPeers string, listenerPort string) (h host.Host, kademliaDHT *dht.IpfsDHT, err error) {
	listenAddr := fmt.Sprintf("/ip4/0.0.0.0/tcp/%s", listenerPort)

	// 1. Create a new resource manager with custom limits.
	scalingLimits := rcmgr.DefaultLimits
	limitsCfg := rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			StreamsOutbound: rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
			Streams:         rcmgr.Unlimited,
			Conns:           rcmgr.Unlimited,
			ConnsOutbound:   rcmgr.Unlimited,
			ConnsInbound:    rcmgr.Unlimited,
			FD:              rcmgr.Unlimited,
			Memory:          rcmgr.LimitVal64(rcmgr.Unlimited),
		},
		Transient: rcmgr.ResourceLimits{
			StreamsOutbound: rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
			Streams:         rcmgr.Unlimited,
			Conns:           rcmgr.Unlimited,
			ConnsOutbound:   rcmgr.Unlimited,
			ConnsInbound:    rcmgr.Unlimited,
			FD:              rcmgr.Unlimited,
			Memory:          rcmgr.LimitVal64(rcmgr.Unlimited),
		},
	}
	limiter := rcmgr.NewFixedLimiter(limitsCfg.Build(scalingLimits.AutoScale()))
	rscMgr, err := rcmgr.NewResourceManager(limiter, rcmgr.WithMetricsDisabled())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create resource manager: %w", err)
	}

	// 2. Create the connection manager
	cm, err := connmgr.NewConnManager(
		config.SettingsObj.ConnManagerLowWater,
		config.SettingsObj.ConnManagerHighWater,
		connmgr.WithGracePeriod(time.Minute),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create connection manager: %w", err)
	}

	// 3. Build the libp2p host
	h, err = libp2p.New(
		libp2p.ListenAddrStrings(listenAddr),
		libp2p.ResourceManager(rscMgr),
		libp2p.ConnectionManager(cm),
		libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			kademliaDHT, err = dht.New(ctx, h, dht.Mode(dht.ModeClient))
			return kademliaDHT, err
		}),
		libp2p.Security(noise.ID, noise.New),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.Transport(tcp.NewTCPTransport),
	)
	if err != nil {
		return
	}

	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		return
	}
	log.Infof("Collector DHT routing table size: %d", kademliaDHT.RoutingTable().Size()) // ADDED

	if bootstrapPeers != "" {
		ConnectToBootstrapPeers(ctx, h, bootstrapPeers)
	}

	log.Infof("Libp2p host created with ID: %s, listening on: %v", h.ID(), h.Addrs())

	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			log.Infof("Collector Peer connected: %s, Addr: %s", conn.RemotePeer(), conn.RemoteMultiaddr())
		},
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			log.Infof("Collector Peer disconnected: %s, Addr: %s", conn.RemotePeer(), conn.RemoteMultiaddr())
		},
	})

	return
}

// ConnectToBootstrapPeers connects the host to a list of bootstrap peers.
func ConnectToBootstrapPeers(ctx context.Context, h host.Host, peers string) {
	peerStrings := strings.Split(peers, ",")
	var wg sync.WaitGroup
	for _, peerString := range peerStrings {
		if peerString == "" {
			continue
		}
		wg.Add(1)
		go func(peerString string) {
			defer wg.Done()
			addr, err := multiaddr.NewMultiaddr(peerString)
			if err != nil {
				log.Errorf("Failed to parse multiaddr: %v", err)
				return
			}
			peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
			if err != nil {
				log.Errorf("Failed to get peer info from multiaddr: %v", err)
				return
			}
			if err := h.Connect(ctx, *peerInfo); err != nil {
				log.Errorf("Failed to connect to bootstrap peer %s: %v", peerString, err)
			} else {
				log.Infof("Successfully connected to bootstrap peer: %s", peerString)
			}
		}(peerString)
	}
	wg.Wait()
}
