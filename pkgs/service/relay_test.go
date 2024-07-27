package service

import (
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	"log"
	"proto-snapshot-server/config"
	"testing"
)

func TestConnectToSequencerP2P(t *testing.T) {
	// Mock relayers data
	relayers := []Relayer{
		{
			ID:              "QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm",
			Name:            "Relayer1",
			RendezvousPoint: "Relayer_POP_test_simulation_phase_1",
			Maddr:           "/ip4/104.248.63.86/tcp/5001/p2p/QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm",
		},
		{
			ID:              "QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj",
			Name:            "Relayer2",
			RendezvousPoint: "Relayer_POP_test_simulation_phase_1",
			Maddr:           "/ip4/137.184.132.196/tcp/5001/p2p/QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj",
		},
	}

	// Initialize the configuration settings
	config.SettingsObj = &config.Settings{
		SequencerId: "QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx",
	}

	host, err := libp2p.New()
	if err != nil {
		t.Fatalf("Failed to create host: %v", err)
	}
	defer host.Close()
	// Test the function
	result := ConnectToSequencerP2P(relayers, host)

	if !result {
		t.Error("expected true, got false")
	}

	log.Println("Connected to peer 1: ", host.Network().ConnsToPeer(peer.ID(relayers[0].ID)))
	log.Println("Connected to peer 2: ", host.Network().ConnsToPeer(peer.ID(relayers[0].ID)))
	log.Println("Connected to peer 3: ", host.Network().ConnsToPeer(peer.ID(config.SettingsObj.SequencerId)))
}
