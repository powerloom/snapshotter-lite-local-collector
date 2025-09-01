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
	dht "github.com/libp2p/go-libp2p-kad-dht"
	logging "github.com/ipfs/go-log/v2"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/powerloom/snapshot-sequencer-validator/pkgs/gossipconfig"
)

type ServiceDependencies struct {
	hostConn    host.Host
	sequencerID peer.ID
	streamPool  *StreamPool
	dht         *dht.IpfsDHT
	initialized bool
	mu          sync.RWMutex
}

var (
	deps       ServiceDependencies
	grpcServer *grpc.Server
	gossiper   *pubsub.PubSub
	logger     = logging.Logger("snapshotter-collector")
)

func InitializeService() error {
	deps.mu.Lock()
	defer deps.mu.Unlock()

	// Set libp2p logging to debug
	logging.SetAllLoggers(logging.LevelInfo)
	logger.Debug("Libp2p logging set to info level")

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

	// Configure DHT for peer discovery
	deps.dht = ConfigureDHT(context.Background(), deps.hostConn)
	if deps.dht == nil {
		return fmt.Errorf("failed to configure DHT")
	}

	var err error
	
	// Get standardized gossipsub parameters for snapshot submissions mesh
	gossipParams, peerScoreParams, peerScoreThresholds := gossipconfig.ConfigureSnapshotSubmissionsMesh(deps.hostConn.ID())
	
	log.Info("Using standardized gossipsub mesh parameters from gossipconfig package")
	
	
	
	// Configure gossipsub with standardized parameters matching other components
	gossiper, err = pubsub.NewGossipSub(
		context.Background(), 
		deps.hostConn,
		// Gossipsub protocol parameters
		pubsub.WithGossipSubParams(*gossipParams),
		
		// Peer scoring configuration
		pubsub.WithPeerScore(peerScoreParams, peerScoreThresholds),
		
		// Discovery configuration
		pubsub.WithDiscovery(routing.NewRoutingDiscovery(deps.dht)),
		
		// Publishing configuration
		pubsub.WithFloodPublish(true),        // Flood to all peers for redundancy
		
		// Message signing policy - consistent with other components
		pubsub.WithMessageSignaturePolicy(pubsub.StrictSign),
	)
	if err != nil {
		return fmt.Errorf("failed to create pubsub: %w", err)
	}
	
	log.Info("Initialized gossipsub with standardized snapshot submissions mesh parameters")
	log.Debug("Configuration: Using gossipconfig package with anti-pruning optimizations")

	// Initialize stream pool
	if err := InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize); err != nil {
		return fmt.Errorf("failed to initialize stream pool: %w", err)
	}

	deps.streamPool = GetLibp2pStreamPool()
	deps.initialized = true

	log.Info("Service initialization complete with sequencer ID: ", deps.sequencerID.String())
	return nil
}
