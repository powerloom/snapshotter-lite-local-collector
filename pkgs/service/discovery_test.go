package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"proto-snapshot-server/config"
	"testing"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
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
	relayers := fetchTrustedRelayers("https://raw.githubusercontent.com/PowerLoom/snapshotter-lite-local-collector/feat/trusted-relayers/relayers.json")

	// Expected result
	expected := []Relayer{
		{
			ID:              "QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm",
			Name:            "Relayer1",
			RendezvousPoint: "Relayer_POP_test_simulation_phase_1",
			Maddr:           "/ip4/104.248.63.86/tcp/9100/p2p/QmQSEao6C3SuPZ8cWiYccPqsd7LtWBTzNgXQZiAjeGTQpm",
		},
		{
			ID:              "QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj",
			Name:            "Relayer2",
			RendezvousPoint: "Relayer_POP_test_simulation_phase_1",
			Maddr:           "/ip4/137.184.132.196/tcp/9100/p2p/QmU3xwsjRqQR4pjJQ7Cxhcb2tiPvaJ6Z5AHDULq7hHWvvj",
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

	config.SettingsObj = &config.Settings{
		TrustedRelayersListUrl: ts.URL,
	}

	host, err := libp2p.New()
	if err != nil {
		t.Fatalf("Failed to create host: %v", err)
	}
	defer host.Close()

	relayers := ConnectToTrustedRelayers(context.Background(), host)

	//time.Sleep(3 * time.Second)

	log.Println(host.Network().Connectedness(peer.ID(relayers[0].ID)))
	log.Println(host.Network().Connectedness(peer.ID(relayers[1].ID)))

	host.Connect(context.Background(), peer.AddrInfo{
		ID:    peer.ID("QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx"),
		Addrs: []ma.Multiaddr{ma.StringCast("/ip4/159.223.164.169/tcp/9100/p2p/QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx")},
	})

	//time.Sleep(3 * time.Second)

	log.Println(host.Network().Connectedness(peer.ID("QmdJbNsbHpFseUPKC9vLt4vMsfdxA4dyHPzsAWuzYz3Yxx")))
	// TODO: FINISH THIS TEST
}

func TestFetchSequencer(t *testing.T) {
	// Create mock data
	sequencers := []Sequencer{
		{
			ID:                "QmTK9e9QNEotPkjWAdZT5bbYKV7PEJVu7iXzdVn3VZDEk9",
			Maddr:             "/dns/proto-snapshot-listener.aws2.powerloom.io/tcp/9100/p2p/QmTK9e9QNEotPkjWAdZT5bbYKV7PEJVu7iXzdVn3VZDEk9",
			DataMarketAddress: "0xa8D4C62BD8831bca08C9a16b3e76C824c9658eA1",
			Environment:       "prod",
		},
		{
			ID:                "QmbWC2TKXDWnYB1picmYwAMRUz7ACXLXDWyLibjHnaRyoN",
			Maddr:             "/dns/proto-snapshot-listener.aws2.powerloom.io/tcp/9100/p2p/QmbWC2TKXDWnYB1picmYwAMRUz7ACXLXDWyLibjHnaRyoN",
			DataMarketAddress: "0x718b3f7B8ef6abA1D1440A26F92AC11EE167005a",
			Environment:       "staging",
		},
	}

	// Convert mock data to JSON
	jsonData, err := json.Marshal(sequencers)
	assert.NoError(t, err)

	t.Log(string(jsonData))

	// Create a test server with the mock JSON response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(jsonData)
	}))
	defer server.Close()

	tests := []struct {
		name           string
		dataMarketAddr string
		environment    string
		expectedID     string
		expectedError  error
	}{
		{
			name:           "Valid sequencer found (prod)",
			dataMarketAddr: "0xa8D4C62BD8831bca08C9a16b3e76C824c9658eA1",
			environment:    "prod",
			expectedID:     "QmTK9e9QNEotPkjWAdZT5bbYKV7PEJVu7iXzdVn3VZDEk9",
			expectedError:  nil,
		},
		{
			name:           "Valid sequencer found (staging)",
			dataMarketAddr: "0x718b3f7B8ef6abA1D1440A26F92AC11EE167005a",
			environment:    "staging",
			expectedID:     "QmbWC2TKXDWnYB1picmYwAMRUz7ACXLXDWyLibjHnaRyoN",
			expectedError:  nil,
		},
		{
			name:           "Sequencer not found",
			dataMarketAddr: "0x9999",
			environment:    "prod",
			expectedID:     "",
			expectedError:  errors.New("Sequencer not found"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sequencer, err := fetchSequencer(server.URL, tt.dataMarketAddr)

			if tt.expectedError != nil {
				assert.Error(t, err)
				assert.Equal(t, tt.expectedError.Error(), err.Error())
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedID, sequencer.ID)
			}
		})
	}
}
