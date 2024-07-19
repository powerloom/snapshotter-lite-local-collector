package service

import (
	"context"
	"github.com/cenkalti/backoff/v4"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	log "github.com/sirupsen/logrus"
	"sync"
)

func isVisited(id peer.ID, visited []peer.ID) bool {
	for _, v := range visited {
		if v == id {
			return true
		}
	}
	return false
}

func ConnectToPeer(ctx context.Context, routingDiscovery *routing.RoutingDiscovery, rendezvousString string, host host.Host, visited []peer.ID) peer.ID {
	peerChan, err := routingDiscovery.FindPeers(ctx, rendezvousString)
	if err != nil {
		log.Fatalf("Failed to find peers: %s", err)
	}
	log.Debugln("Triggering peer discovery")

	log.Debugln("Skipping visited peers: ", visited)

	for relayer := range peerChan {
		if relayer.ID == host.ID() || isVisited(relayer.ID, visited) {
			continue // Skip self or peers with no addresses
		}

		if host.Network().Connectedness(relayer.ID) != network.Connected {
			// Connect to the relayer if not already connected
			if err = backoff.Retry(func() error { return host.Connect(ctx, relayer) }, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 1)); err != nil {
				log.Errorf("Failed to connect to relayer %s: %s", relayer.ID, err)
			} else {
				log.Infof("Connected to new relayer: %s", relayer.ID)
				return relayer.ID
			}
		} else {
			log.Debugln("Already connected to: ", relayer.ID)
			return relayer.ID
		}
	}
	log.Debugln("Active connections: ", activeConnections)
	return ""
}

func ConfigureDHT(ctx context.Context, host host.Host) *dht.IpfsDHT {
	// Set up a Kademlia DHT for the service host
	kademliaDHT, err := dht.New(ctx, host)
	if err != nil {
		log.Fatalf("Failed to create DHT: %s", err)
	}

	// Bootstrap the DHT
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		log.Fatalf("Failed to bootstrap DHT: %s", err)
	}

	var wg sync.WaitGroup
	for _, peerAddr := range dht.DefaultBootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := host.Connect(ctx, *peerinfo); err != nil {
				log.Warning(err)
			} else {
				log.Debugln("Connection established with bootstrap node:", *peerinfo)
			}
		}()
	}
	wg.Wait()

	return kademliaDHT
}
