package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"proto-snapshot-server/config"
	"sync"
	"time"

	"github.com/pkg/errors"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
)

type Relayer struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	RendezvousPoint string `json:"rendezvousPoint"`
	Maddr           string `json:"maddr"`
}

type Sequencer struct {
	ID                string `json:"id"`
	Maddr             string `json:"maddr"`
	DataMarketAddress string `json:"dataMarketAddress"`
	Environment       string `json:"environment"`
}

func fetchSequencer(url string, dataMarketAddress string) (Sequencer, error) {
	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("Failed to fetch JSON: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("Failed to read response body: %v", err)
	}

	var sequencers []Sequencer
	err = json.Unmarshal(body, &sequencers)
	if err != nil {
		log.Debugln("Failed to unmarshal JSON:", err)
	}

	for _, sequencer := range sequencers {
		log.Debugf(
			"ID: %s, Maddr: %s, Data Market Address: %s, Environment: %s\n",
			sequencer.ID,
			sequencer.Maddr,
			sequencer.DataMarketAddress,
			sequencer.Environment,
		)

		if sequencer.DataMarketAddress == dataMarketAddress {
			return sequencer, nil
		}
	}

	return Sequencer{}, errors.New("Sequencer not found")
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

func AddPeerConnection(ctx context.Context, host host.Host, relayerAddr string) bool {
	stableRelayerMA, err := ma.NewMultiaddr(relayerAddr)
	if err != nil {
		log.Debugln("Failed to parse stable peer multiaddress: ", err)
	}

	peerInfo, err := peer.AddrInfoFromP2pAddr(stableRelayerMA)
	if err != nil {
		log.Debugln("Failed to extract peer info from multiaddress:", err)
	}

	if host.Network().Connectedness(peerInfo.ID) == network.Connected {
		log.Debugln("Skipping connected relayer: ", peerInfo.ID)
		return true
	}

	err = host.Connect(ctx, *peerInfo)
	if err != nil {
		log.Errorf("Failed to connect to relayer %s: %s", peerInfo.ID, err)
		return false
	} else {
		log.Infof("Connected to new relayer: %s", peerInfo.ID)
		log.Infoln("Connected: ", host.Network().ConnsToPeer(peerInfo.ID))
		return true
	}
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

func ReconnectToSequencer() error {
	log.Info("Attempting to reconnect to sequencer...")
	// Assuming GetSequencerConnection is defined elsewhere and returns the host and sequencer ID
	hostConn, seqId, err := GetSequencerConnection()
	if err != nil {
		return fmt.Errorf("failed to get sequencer connection details: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Attempt to connect to the sequencer peer
	if err := hostConn.Connect(ctx, peer.AddrInfo{ID: seqId}); err != nil {
		log.Errorf("Failed to reconnect to sequencer: %v", err)
		return err
	}

	log.Info("Successfully reconnected to sequencer")
	return nil
}
