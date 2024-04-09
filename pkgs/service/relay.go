package service

import (
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
	"proto-snapshot-server/config"
	"sync"
	"time"
)

var rpctorelay host.Host
var CollectorId peer.ID

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
	kademliaDHT, err := dht.New(context.Background(), rpctorelay)
	if err != nil {
		log.Fatalf("Failed to create DHT: %s", err)
	}

	// Bootstrap the DHT
	if err = kademliaDHT.Bootstrap(context.Background()); err != nil {
		log.Fatalf("Failed to bootstrap DHT: %s", err)
	}

	var wg sync.WaitGroup
	for _, peerAddr := range dht.DefaultBootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := rpctorelay.Connect(context.Background(), *peerinfo); err != nil {
				log.Warning(err)
			} else {
				log.Debugln("Connection established with bootstrap node:", *peerinfo)
			}
		}()
	}
	wg.Wait()

	routingDiscovery := routing.NewRoutingDiscovery(kademliaDHT)

	// Find peers advertising under the same rendezvous string
	peerChan, err := routingDiscovery.FindPeers(context.Background(), config.SettingsObj.RendezvousPoint)
	if err != nil {
		log.Fatalf("Failed to find peers: %s", err)
	}

	var peerId string

	for peer := range peerChan {
		if peer.ID == rpctorelay.ID() || len(peer.Addrs) == 0 {
			// Skip self or peers with no addresses
			continue
		}

		log.Debugf("Found peer: %s/%s\n", peer.Addrs[0], peer.ID.String())

		// Connect to the peer
		if err := rpctorelay.Connect(context.Background(), peer); err != nil {
			log.Printf("Failed to connect to peer %s: %s", peer.ID.String(), err)
		} else {
			if err = rpctorelay.Connect(ctx, peer); err != nil {
				log.Debugln("Failed to connect grpc server to relayer")
			}
			peerId = peer.ID.String()

			fmt.Printf("Connected to peer: %s\n", peer.ID.String())
		}
	}

	var collectorAddr ma.Multiaddr

	collectorAddr, err = ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", peerId, config.SettingsObj.CollectorId))
	if err != nil {
		fmt.Println(err.Error())
		return
	}

	collectorInfo, err := peer.AddrInfoFromP2pAddr(collectorAddr)
	CollectorId = collectorInfo.ID

	if err != nil {
		fmt.Println(err)
	}

	if err := rpctorelay.Connect(ctx, *collectorInfo); err != nil {
		log.Debugln("Failed to connect to the Collector:", err)
	} else {
		log.Debugln("Successfully connected to the Collector: ", collectorAddr.String())
	}
}
