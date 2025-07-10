package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"sync"
	"time"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

type ServiceDependencies struct {
	hostConn    host.Host
	sequencerID peer.ID
	streamPool  *StreamPool
	initialized bool
	mu          sync.RWMutex
}

var (
	deps       ServiceDependencies
	grpcServer *grpc.Server
	gossiper   *pubsub.PubSub
)

func InitializeService() error {
	deps.mu.Lock()
	defer deps.mu.Unlock()

	if deps.initialized {
		log.Warn("Service already initialized")
		return nil
	}

	// Establish sequencer connection
	if err := EstablishSequencerConnection(); err != nil {
		return fmt.Errorf("failed to establish sequencer connection: %w", err)
	}

	// Verify connection state
	if SequencerHostConn == nil {
		return fmt.Errorf("sequencer host connection not initialized")
	}

	if SequencerID.String() == "" {
		return fmt.Errorf("sequencer ID not initialized")
	}

	deps.hostConn = SequencerHostConn
	deps.sequencerID = SequencerID

	// Give DHT some time to bootstrap and discover peers
	log.Info("Waiting 30 seconds for DHT to discover peers...")
	time.Sleep(30 * time.Second)

	var err error
	gossiper, err = pubsub.NewGossipSub(context.Background(), deps.hostConn)
	if err != nil {
		return fmt.Errorf("failed to create pubsub: %w", err)
	}

	// Configure DHT for peer discovery
	dhtInstance := ConfigureDHT(context.Background(), deps.hostConn)
	if dhtInstance == nil {
		return fmt.Errorf("failed to configure DHT")
	}

	// Initialize stream pool
	if err := InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize); err != nil {
		return fmt.Errorf("failed to initialize stream pool: %w", err)
	}

	deps.streamPool = GetLibp2pStreamPool()
	deps.initialized = true

	log.Info("Service initialization complete with sequencer ID: ", deps.sequencerID.String())
	return nil
}
