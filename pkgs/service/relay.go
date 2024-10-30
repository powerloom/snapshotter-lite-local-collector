package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"sync"
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

var (
	SequencerHostConn host.Host
	SequencerId       peer.ID
	sequencerMu       sync.RWMutex
	connManager       *connmgr.BasicConnMgr
	tcpAddr           ma.Multiaddr
	rm                network.ResourceManager

	routingDiscovery *routing.RoutingDiscovery

	activeConnections int
)

func handleConnectionEstablished(network network.Network, conn network.Conn) {
	activeConnections++
}

func handleConnectionClosed(network network.Network, conn network.Conn) {
	activeConnections--
}

// Safe getter for sequencer connection and ID
func GetSequencerConnection() (host.Host, peer.ID) {
	sequencerMu.RLock()
	defer sequencerMu.RUnlock()
	return SequencerHostConn, SequencerId
}

func ConfigureRelayer() error {
	sequencerMu.Lock()
	defer sequencerMu.Unlock()

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

func ReConnectToSequencer() error {
	sequencerMu.Lock()
	defer sequencerMu.Unlock()

	if SequencerId.String() == "" {
		return fmt.Errorf("cannot connect: sequencer ID not initialized")
	}

	// Clear existing connections first
	if SequencerHostConn != nil {
		log.Info("Clearing existing connection")
		if err := SequencerHostConn.Close(); err != nil {
			log.Warnf("Error closing existing connection: %v", err)
		}
	}

	// Reconfigure the connection
	var err error
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

	// After reconnection, rebuild stream pool
	if err := RebuildStreamPool(); err != nil {
		return fmt.Errorf("failed to rebuild stream pool after reconnection: %w", err)
	}

	log.Infof("Successfully reconnected to sequencer and rebuilt stream pool")
	return nil
}
