package service

import (
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
	"proto-snapshot-server/config"
	"time"
)

var rpctorelay host.Host
var SequencerId peer.ID
var routingDiscovery *routing.RoutingDiscovery

var activeConnections int

func handleConnectionEstablished(network network.Network, conn network.Conn) {
	activeConnections++
}

func handleConnectionClosed(network network.Network, conn network.Conn) {
	activeConnections--
}

func ConfigureRelayer() {
	var err error
	tcpAddr, _ := ma.NewMultiaddr("/ip4/0.0.0.0/tcp/9000")

	connManager, _ := connmgr.NewConnManager(
		100,
		400,
		connmgr.WithGracePeriod(5*time.Minute))

	scalingLimits := rcmgr.DefaultLimits

	libp2p.SetDefaultServiceLimits(&scalingLimits)

	scaledDefaultLimits := scalingLimits.AutoScale()

	cfg := rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			Streams:         rcmgr.Unlimited,
			StreamsOutbound: rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
			Conns:           rcmgr.Unlimited,
			ConnsInbound:    rcmgr.Unlimited,
			ConnsOutbound:   rcmgr.Unlimited,
			FD:              rcmgr.Unlimited,
			Memory:          rcmgr.LimitVal64(rcmgr.Unlimited),
		},
	}

	limits := cfg.Build(scaledDefaultLimits)

	limiter := rcmgr.NewFixedLimiter(limits)

	rm, err := rcmgr.NewResourceManager(limiter, rcmgr.WithMetricsDisabled())

	rpctorelay, err = libp2p.New(
		libp2p.EnableRelay(),
		libp2p.ConnectionManager(connManager),
		libp2p.ListenAddrs(tcpAddr),
		libp2p.ResourceManager(rm),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.Security(noise.ID, noise.New),
		libp2p.DefaultTransports,
		libp2p.NATPortMap(),
		libp2p.EnableRelayService(),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport))

	if err != nil {
		log.Debugln("Error instantiating libp2p host: ", err.Error())
		return
	}

	log.Debugln("id: ", rpctorelay.ID().String())
	rpctorelay.Network().Notify(&network.NotifyBundle{
		ConnectedF:    handleConnectionEstablished,
		DisconnectedF: handleConnectionClosed,
	})

	// Set up a Kademlia DHT for the service host

	kademliaDHT := ConfigureDHT(context.Background(), rpctorelay)

	routingDiscovery = routing.NewRoutingDiscovery(kademliaDHT)

	util.Advertise(context.Background(), routingDiscovery, config.SettingsObj.ClientRendezvousPoint)

	time.Sleep(2 * time.Minute)
	peerId := ConnectToPeer(context.Background(), routingDiscovery, config.SettingsObj.RelayerRendezvousPoint, rpctorelay, nil)

	var sequencerAddr ma.Multiaddr

	sequencerAddr, err = ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", peerId, config.SettingsObj.SequencerId))
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	sequencerInfo, err := peer.AddrInfoFromP2pAddr(sequencerAddr)
	SequencerId = sequencerInfo.ID

	if err != nil {
		fmt.Println(err)
	}

	if err := rpctorelay.Connect(context.Background(), *sequencerInfo); err != nil {
		log.Debugln("Failed to connect to the Sequencer:", err)
	} else {
		log.Debugln("Successfully connected to the Sequencer: ", sequencerAddr.String())
	}
}
