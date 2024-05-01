package service

import (
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
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

func ConfigureRelayer() {
	ctx := context.Background()

	var err error

	connManager, _ := connmgr.NewConnManager(
		100,
		400,
		connmgr.WithGracePeriod(time.Minute))

	rpctorelay, err = libp2p.New(
		libp2p.EnableRelay(),
		libp2p.ConnectionManager(connManager),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.Security(noise.ID, noise.New),
		libp2p.DefaultTransports,
		libp2p.NATPortMap(),
		libp2p.EnableRelayService(),
		libp2p.EnableNATService())

	// Set up a Kademlia DHT for the service host

	kademliaDHT := ConfigureDHT(context.Background(), rpctorelay)

	routingDiscovery = routing.NewRoutingDiscovery(kademliaDHT)

	util.Advertise(context.Background(), routingDiscovery, config.SettingsObj.ClientRendezvousPoint)

	time.Sleep(time.Minute)
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

	if err := rpctorelay.Connect(ctx, *sequencerInfo); err != nil {
		log.Debugln("Failed to connect to the Sequencer:", err)
	} else {
		log.Debugln("Successfully connected to the Sequencer: ", sequencerAddr.String())
	}
}
