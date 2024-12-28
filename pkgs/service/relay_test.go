package service

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"proto-snapshot-server/config"
	"testing"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestConnectToSequencerP2P(t *testing.T) {
	// Mock relayers data
	mockResponse := `[{
		"id": "QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm",
		"name": "Relayer1",
		"rendezvousPoint": "Relayer_POP_test_simulation_phase_1",
		"maddr": "/ip4/104.248.63.86/tcp/9100/p2p/QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm"
	}, {
		"id": "QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj",
		"name": "Relayer2",
		"rendezvousPoint": "Relayer_POP_test_simulation_phase_1",
		"maddr": "/ip4/137.184.132.196/tcp/9100/p2p/QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj"
	}]`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, mockResponse)
	}))

	defer ts.Close()

	// Initialize the configuration settings
	config.SettingsObj = &config.Settings{
		TrustedRelayersListUrl: ts.URL,
		SequencerID:            "QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx",
	}

	host, err := libp2p.New()
	if err != nil {
		t.Fatalf("Failed to create host: %v", err)
	}
	defer host.Close()

	// Fetch the relayers
	relayers := ConnectToTrustedRelayers(context.Background(), host)

	// Test the function
	result := ConnectToSequencerP2P(relayers, host)
	if !result {
		t.Error("expected true, got false")
	}

	log.Println("Connected to peer 1: ", host.Network().ConnsToPeer(peer.ID(relayers[0].ID)))
	log.Println("Connected to peer 2: ", host.Network().ConnsToPeer(peer.ID(relayers[0].ID)))
	log.Println("Connected to peer 3: ", host.Network().ConnsToPeer(peer.ID(config.SettingsObj.SequencerID)))
}
