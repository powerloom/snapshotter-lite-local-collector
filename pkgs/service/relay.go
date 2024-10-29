package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"time"

	circuitv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"

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
)

var SequencerHostConn host.Host
var SequencerId peer.ID
var routingDiscovery *routing.RoutingDiscovery

var activeConnections int

func handleConnectionEstablished(network network.Network, conn network.Conn) {
	activeConnections++
}

func handleConnectionClosed(network network.Network, conn network.Conn) {
	activeConnections--
}

func ConfigureRelayer() error {
	var err error
	tcpAddr, _ := ma.NewMultiaddr("/ip4/0.0.0.0/tcp/9000")

	connManager, _ := connmgr.NewConnManager(
		40960,
		81920,
		connmgr.WithGracePeriod(1*time.Minute))

	scalingLimits := rcmgr.DefaultLimits

	libp2p.SetDefaultServiceLimits(&scalingLimits)

	scaledDefaultLimits := scalingLimits.AutoScale()

	cfg := rcmgr.PartialLimitConfig{
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
	}

	limits := cfg.Build(scaledDefaultLimits)

	limiter := rcmgr.NewFixedLimiter(limits)

	rm, err := rcmgr.NewResourceManager(limiter, rcmgr.WithMetricsDisabled())

	if err != nil {
		log.Debugln("Error instantiating resource manager: ", err.Error())
		return err
	}

	SequencerHostConn, err = libp2p.New(
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
		return err
	}

	log.Debugln("id: ", SequencerHostConn.ID().String())
	SequencerHostConn.Network().Notify(&network.NotifyBundle{
		ConnectedF:    handleConnectionEstablished,
		DisconnectedF: handleConnectionClosed,
	})

	// Set up a Kademlia DHT for the service host

	kademliaDHT := ConfigureDHT(context.Background(), SequencerHostConn)

	routingDiscovery = routing.NewRoutingDiscovery(kademliaDHT)

	util.Advertise(context.Background(), routingDiscovery, config.SettingsObj.ClientRendezvousPoint)

	// peerId := ConnectToPeer(context.Background(), routingDiscovery, config.SettingsObj.RelayerRendezvousPoint, SequencerHostConn, nil)
	// if peerId == "" {
	// 	ReportingInstance.SendFailureNotification(nil, "Unable to connect to relayer peers")
	// 	return
	// }
	// ConnectToSequencer(peerId)

	// ConnectToSequencer()
	return nil
}

func ConnectToSequencerP2P(relayers []Relayer, p2pHost host.Host) bool {
	for _, relayer := range relayers {
		relayerMA, err := ma.NewMultiaddr(relayer.Maddr)
		relayerInfo, err := peer.AddrInfoFromP2pAddr(relayerMA)
		if reservation, err := circuitv2.Reserve(context.Background(), p2pHost, *relayerInfo); err != nil {
			log.Fatalf("Failed to request reservation with relay: %v", err)
		} else {
			fmt.Println("Reservation with relay successful", reservation.Expiration, reservation.LimitDuration)
		}
		sequencerAddr, err := ma.NewMultiaddr(fmt.Sprintf("%s/p2p-circuit/p2p/%s", relayer.Maddr, config.SettingsObj.SequencerId))

		if err != nil {
			log.Debugln(err.Error())
		}

		log.Debugln("Connecting to Sequencer: ", sequencerAddr.String())
		isConnected := AddPeerConnection(context.Background(), p2pHost, sequencerAddr.String())
		if isConnected {
			return true
		}
	}
	return false
}

func ConnectToSequencer() error {
	//trustedRelayers := ConnectToTrustedRelayers(context.Background(), SequencerHostConn)
	//isConnectedP2P := ConnectToSequencerP2P(trustedRelayers, SequencerHostConn)
	//if isConnectedP2P {
	//	log.Debugln("Successfully connected to the Sequencer: ", SequencerHostConn.Network().Connectedness(peer.ID(config.SettingsObj.SequencerId)), isConnectedP2P)
	//	return
	//} else {
	//	log.Debugln("Failed to connect to the Sequencer")
	//}

	var sequencerAddr ma.Multiaddr
	var err error

	sequencer, err := fetchSequencer("https://raw.githubusercontent.com/PowerLoom/snapshotter-lite-local-collector/feat/trusted-relayers/sequencers.json", config.SettingsObj.DataMarketAddress)
	if err != nil {
		log.Debugln(err.Error())
	}
	sequencerAddr, err = ma.NewMultiaddr(sequencer.Maddr)
	if err != nil {
		log.Debugln(err.Error())
		return err
	}

	sequencerInfo, err := peer.AddrInfoFromP2pAddr(sequencerAddr)

	if err != nil {
		log.Errorln("Error converting MultiAddr to AddrInfo: ", err.Error())
	}

	SequencerId = sequencerInfo.ID

	if err := SequencerHostConn.Connect(context.Background(), *sequencerInfo); err != nil {
		log.Debugln("Failed to connect to the Sequencer:", err)
		return err
	}
	log.Debugln("Successfully connected to the Sequencer: ", sequencerAddr.String())
	return nil
}
