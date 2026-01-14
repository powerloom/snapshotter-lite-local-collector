package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"sync"

	logging "github.com/ipfs/go-log/v2"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/powerloom/snapshot-sequencer-validator/pkgs/gossipconfig"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

type ServiceDependencies struct {
	hostConn       host.Host
	sequencerID    peer.ID
	streamPool     *StreamPool
	dht            *dht.IpfsDHT
	initialized    bool
	serverInstance *server // Reference to server instance for disconnection tracking
	mu             sync.RWMutex
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

	if deps.initialized {
		log.Warn("Service already initialized")
		return nil
	}

	// Set libp2p logging to debug
	logging.SetAllLoggers(logging.LevelInfo)
	logger.Debug("Libp2p logging set to info level")

	// Establish sequencer connection
	if err := EstablishSequencerConnection(); err != nil {
		return fmt.Errorf("failed to establish sequencer connection: %w", err)
	}

	// Verify connection state
	if P2PHost == nil {
		return fmt.Errorf("P2P host not initialized")
	}

	if SequencerID.String() == "" {
		return fmt.Errorf("sequencer ID not initialized")
	}

	deps.hostConn = P2PHost
	deps.sequencerID = SequencerID

	// Log local peer ID
	log.Infof("Local collector peer ID: %s", deps.hostConn.ID().String())

	// Configure DHT for peer discovery
	deps.dht = ConfigureDHT(context.Background(), deps.hostConn)
	if deps.dht == nil {
		return fmt.Errorf("failed to configure DHT")
	}

	// DHT bootstraps asynchronously - no need to wait
	// Peer discovery happens in background via topic discovery and rendezvous points
	log.Debug("DHT configured, bootstrapping in background")

	var err error

	// Get standardized gossipsub parameters for snapshot submissions mesh
	gossipParams, peerScoreParams, peerScoreThresholds, paramHash := gossipconfig.ConfigureSnapshotSubmissionsMesh(deps.hostConn.ID())

	log.Info("Using standardized gossipsub mesh parameters from gossipconfig package")
	log.Infof("🔑 Gossipsub parameter hash: %s (local collector)", paramHash)

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
		pubsub.WithFloodPublish(true), // Flood to all peers for redundancy

		// Message signing policy - consistent with other components
		pubsub.WithMessageSignaturePolicy(pubsub.StrictSign),

		// Buffer/queue configuration - CRITICAL for preventing message drops under high load
		pubsub.WithValidateQueueSize(config.SettingsObj.GossipsubValidateQueueSize),
		pubsub.WithValidateWorkers(config.SettingsObj.GossipsubValidateWorkers),
	)
	if err != nil {
		return fmt.Errorf("failed to create pubsub: %w", err)
	}

	log.Info("Initialized gossipsub with standardized snapshot submissions mesh parameters")
	log.Debug("Configuration: Using gossipconfig package with anti-pruning optimizations")
	log.Infof("Gossipsub buffer configuration: validate queue size=%d, validate workers=%d",
		config.SettingsObj.GossipsubValidateQueueSize, config.SettingsObj.GossipsubValidateWorkers)

	// Initialize stream pool only if centralized sequencer is enabled
	if config.SettingsObj.CentralizedSequencerEnabled {
		if err := InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize); err != nil {
			return fmt.Errorf("failed to initialize stream pool: %w", err)
		}
		deps.streamPool = GetLibp2pStreamPool()
		log.Info("Stream pool initialized for centralized sequencer submissions")
	} else {
		log.Info("Centralized sequencer disabled - skipping stream pool initialization")
		deps.streamPool = nil
	}

	deps.initialized = true

	log.Info("Service initialization complete with sequencer ID: ", deps.sequencerID.String())
	return nil
}
