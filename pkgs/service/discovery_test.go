package service

import (
	"context"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"net/http/httptest"
	"proto-snapshot-server/config"
	"testing"
	"time"
)

type Settings struct {
	TrustedRelayersListUrl string
}

var SettingsObj *Settings

func TestFetchTrustedRelayers(t *testing.T) {
	// Sample JSON response
	mockResponse := `[{
		"id": "QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm",
		"name": "Relayer1",
		"rendezvousPoint": "Relayer_POP_test_simulation_phase_1",
		"maddr": "/ip4/104.248.63.86/tcp/5001/p2p/QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm"
	}, {
		"id": "QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj",
		"name": "Relayer2",
		"rendezvousPoint": "Relayer_POP_test_simulation_phase_1",
		"maddr": "/ip4/137.184.132.196/tcp/5001/p2p/QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj"
	}]`

	// Create a test server that returns the mock JSON response
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, mockResponse)
	}))
	defer ts.Close()

	// Call the function to test
	relayers := fetchTrustedRelayers(ts.URL)

	// Expected result
	expected := []Relayer{
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

	if len(relayers) != len(expected) {
		t.Fatalf("expected %d relayers, got %d", len(expected), len(relayers))
	}

	for i, r := range relayers {
		if r != expected[i] {
			t.Errorf("expected relayer %v, got %v", expected[i], r)
		}
	}
}

func TestAddPeerConnection(t *testing.T) {
	host, err := libp2p.New()
	if err != nil {
		t.Fatalf("Failed to create host: %v", err)
	}
	defer host.Close()
	ctx := context.Background()
	relayerAddr := "/ip4/104.248.63.86/tcp/5001/p2p/QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm"

	result := AddPeerConnection(ctx, host, relayerAddr)

	if !result {
		t.Error("expected true, got false")
	}
}

func TestConnectToTrustedRelayers(t *testing.T) {
	mockResponse := `[{
		"id": "QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm",
		"name": "Relayer1",
		"rendezvousPoint": "Relayer_POP_test_simulation_phase_1",
		"maddr": "/ip4/104.248.63.86/tcp/5001/p2p/QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm"
	}, {
		"id": "QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj",
		"name": "Relayer2",
		"rendezvousPoint": "Relayer_POP_test_simulation_phase_1",
		"maddr": "/ip4/137.184.132.196/tcp/5001/p2p/QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj"
	}]`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, mockResponse)
	}))

	defer ts.Close()

	config.SettingsObj = &config.Settings{
		TrustedRelayersListUrl: ts.URL,
	}

	host, err := libp2p.New()
	if err != nil {
		t.Fatalf("Failed to create host: %v", err)
	}
	defer host.Close()

	relayers := ConnectToTrustedRelayers(context.Background(), host)

	time.Sleep(3 * time.Second)

	log.Println(host.Network().Connectedness(peer.ID(relayers[0].ID)))
	log.Println(host.Network().Connectedness(peer.ID(relayers[1].ID)))

	host.Connect(context.Background(), peer.AddrInfo{
		ID:    peer.ID("QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx"),
		Addrs: []ma.Multiaddr{ma.StringCast("/ip4/159.223.164.169/tcp/9100/p2p/QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx")},
	})

	time.Sleep(3 * time.Second)

	log.Println(host.Network().Connectedness(peer.ID("QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx")))
	// TODO: FINISH THIS TEST
}
