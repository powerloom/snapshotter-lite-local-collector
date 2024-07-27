package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"proto-snapshot-server/config"
	"sync"

	"github.com/cenkalti/backoff/v4"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
)

type Relayer struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	RendezvousPoint string `json:"rendezvousPoint"`
	Maddr           string `json:"maddr"`
}

func fetchTrustedRelayers(url string) []Relayer {
	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("Failed to fetch JSON: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("Failed to read response body: %v", err)
	}

	var relayers []Relayer
	err = json.Unmarshal(body, &relayers)
	if err != nil {
		log.Debugln("Failed to unmarshal JSON:", err)
	}

	for _, relayer := range relayers {
		log.Debugf("ID: %s, Name: %s, Rendezvous Point: %s, Maddr: %s\n", relayer.ID, relayer.Name, relayer.RendezvousPoint, relayer.Maddr)
	}

	return relayers
}

func isVisited(id peer.ID, visited []peer.ID) bool {
	for _, v := range visited {
		if v == id {
			return true
		}
	}
	return false
}

func AddPeerConnection(ctx context.Context, host host.Host, relayerAddr string) bool {
	stableRelayerMA, err := ma.NewMultiaddr(relayerAddr)
	if err != nil {
		log.Debugln("Failed to parse stable peer multiaddress: ", err)
	}

	peerInfo, err := peer.AddrInfoFromP2pAddr(stableRelayerMA)
	if err != nil {
		log.Debugln("Failed to extract peer info from multiaddress:", err)
	}

	if host.Network().Connectedness(peerInfo.ID) != network.Connected {
		log.Debugln("Skipping connected relayer: ", peerInfo.ID)
		return true
	}

	// Add the peer to the peerstore
	host.Peerstore().AddAddrs(peerInfo.ID, peerInfo.Addrs, peerstore.PermanentAddrTTL)

	if err := backoff.Retry(func() error { return host.Connect(ctx, *peerInfo) }, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 1)); err != nil {
		log.Errorf("Failed to connect to relayer %s: %s", peerInfo.ID, err)
	} else {
		log.Infof("Connected to new relayer: %s", peerInfo.ID)
		return true
	}

	return false
}

func ConnectToTrustedRelayers(ctx context.Context, host host.Host) []Relayer {
	relayers := fetchTrustedRelayers(config.SettingsObj.TrustedRelayersListUrl)
	var connectedRelayers []Relayer

	for _, relayer := range relayers {
		if AddPeerConnection(ctx, host, relayer.Maddr) {
			connectedRelayers = append(connectedRelayers, relayer)
		}
	}

	return connectedRelayers
}

func ConnectToPeer(ctx context.Context, routingDiscovery *routing.RoutingDiscovery, rendezvousString string, host host.Host, visited []peer.ID) peer.ID {
	//stableRelayer1 := "/ip4/104.248.63.86/tcp/5001/p2p/QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm"
	//stableRelayer2 := "/ip4/137.184.132.196/tcp/5001/p2p/QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj"

	peerChan, err := routingDiscovery.FindPeers(ctx, rendezvousString)

	if err != nil {
		log.Fatalf("Failed to find peers: %s", err)
	}
	log.Debugln("Triggering peer discovery")

	log.Debugln("Skipping visited peers: ", visited)

	for relayer := range peerChan {
		if relayer.ID == host.ID() {
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
			// return relayer.ID
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
