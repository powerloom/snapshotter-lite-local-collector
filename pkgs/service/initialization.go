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
	logging "github.com/ipfs/go-log"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
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
	
	// Configure custom gossipsub parameters optimized for publish-heavy nodes
	// These settings prevent pruning of nodes that primarily publish
	gossipParams := pubsub.DefaultGossipSubParams()
	
	// Increase mesh degree targets to maintain more connections
	gossipParams.D = 8                     // Target mesh degree (default 6)
	gossipParams.Dlo = 6                   // Lower bound for mesh degree (default 5)
	gossipParams.Dhi = 12                  // Upper bound for mesh degree (default 12)
	gossipParams.Dlazy = 8                 // Lazy push/full push threshold (default 6)
	
	// Increase heartbeat interval to reduce churn
	gossipParams.HeartbeatInterval = 2 * time.Second  // Slower heartbeat (default 1s)
	
	// Increase history windows to keep messages longer
	gossipParams.HistoryLength = 10        // Keep more message IDs (default 5)
	gossipParams.HistoryGossip = 5         // Gossip more history (default 3)
	
	// Configure fanout parameters for publishing
	gossipParams.FanoutTTL = 120 * time.Second  // Keep fanout peers longer (default 60s)
	
	// Configure custom peer scoring to be more lenient
	peerScoreParams := &pubsub.PeerScoreParams{
		// Slower decay rates to prevent rapid score degradation
		DecayInterval: 30 * time.Second,      // Much slower decay (default 1s)
		DecayToZero:   0.01,                  // Very slow decay to zero
		
		// App-specific scoring - give all peers a baseline positive score
		AppSpecificScore: func(p peer.ID) float64 {
			// Give ourselves and other publishers a high baseline score
			if p == deps.hostConn.ID() {
				return 1000.0
			}
			return 100.0
		},
		AppSpecificWeight: 1.0,
		
		// IP colocation penalty (disable for testing, enable in production)
		IPColocationFactorThreshold: 10,
		IPColocationFactorWeight:    -5.0,
		
		// Behaviour penalties - make them more lenient
		BehaviourPenaltyWeight:    -1.0,     // Reduced from default -10
		BehaviourPenaltyThreshold:  10.0,    // Increased from default 6
		BehaviourPenaltyDecay:      0.95,    // Slower decay (default 0.9)
		
		// Remove time in mesh penalty for new peers
		RetainScore: 30 * time.Minute,       // Keep scores longer
	}
	
	// Configure topic-specific parameters
	topicScoreParams := &pubsub.TopicScoreParams{
		// Reduce penalties for not delivering messages
		TopicWeight: 0.5,  // Reduce topic importance
		
		// Time in mesh parameters - reward staying in mesh
		TimeInMeshWeight:  0.1,                      // Small positive weight
		TimeInMeshQuantum: 10 * time.Second,         // Slower accumulation
		TimeInMeshCap:     100.0,                    // Cap the benefit
		
		// First message deliveries - reward publishing
		FirstMessageDeliveriesWeight:  1.0,          // Positive weight for publishing
		FirstMessageDeliveriesDecay:   0.95,         // Slow decay
		FirstMessageDeliveriesCap:     100.0,        // Cap the benefit
		
		// Mesh message deliveries - very lenient
		MeshMessageDeliveriesWeight:     -0.1,       // Very small penalty (default -1)
		MeshMessageDeliveriesDecay:      0.95,       // Slow decay
		MeshMessageDeliveriesThreshold:  0.1,        // Very low threshold
		MeshMessageDeliveriesCap:        10.0,       // Low cap on penalty
		MeshMessageDeliveriesActivation: 60 * time.Second,  // Long grace period
		MeshMessageDeliveriesWindow:     30 * time.Second,  // Longer window
		
		// Remove mesh failure penalty
		MeshFailurePenaltyWeight: 0.0,               // No penalty for mesh failures
		MeshFailurePenaltyDecay:  0.0,
		
		// Invalid messages - standard penalties
		InvalidMessageDeliveriesWeight: -100.0,
		InvalidMessageDeliveriesDecay:  0.5,
	}
	
	// Configure score thresholds to be more lenient
	peerScoreThresholds := &pubsub.PeerScoreThresholds{
		GossipThreshold:             -500,    // Much more lenient (default -10)
		PublishThreshold:            -1000,   // Much more lenient (default -50)
		GraylistThreshold:           -2000,   // Much more lenient (default -80)
		AcceptPXThreshold:           0,       // Accept peer exchange from neutral peers
		OpportunisticGraftThreshold: 1,       // Low threshold for grafting
	}
	
	// Set topic score parameters for both topics
	peerScoreParams.Topics = map[string]*pubsub.TopicScoreParams{
		"/powerloom/snapshot-submissions/0":   topicScoreParams,
		"/powerloom/snapshot-submissions/all": topicScoreParams,
	}
	
	// Configure gossipsub with custom parameters
	gossiper, err = pubsub.NewGossipSub(
		context.Background(), 
		deps.hostConn,
		// Discovery configuration
		pubsub.WithDiscovery(routing.NewRoutingDiscovery(deps.dht)),
		
		// Gossipsub protocol parameters
		pubsub.WithGossipSubParams(gossipParams),
		
		// Peer scoring configuration
		pubsub.WithPeerScore(peerScoreParams, peerScoreThresholds),
		
		// Message validation - accept all messages quickly
		pubsub.WithValidateQueueSize(128),
		pubsub.WithValidateWorkers(4),
		
		// Publishing configuration
		pubsub.WithFloodPublish(true),        // Flood to all peers for redundancy
		
		// Peer exchange for discovery
		pubsub.WithPeerExchange(true),
		
		// Direct peers can be added here if needed
		pubsub.WithDirectPeers([]peer.AddrInfo{}),
		
		// Message signing (can be disabled for performance in trusted networks)
		pubsub.WithMessageSigning(true),
		pubsub.WithStrictSignatureVerification(false),
	)
	if err != nil {
		return fmt.Errorf("failed to create pubsub: %w", err)
	}
	
	log.Info("Initialized gossipsub with custom parameters optimized for publishing")

	// Initialize stream pool
	if err := InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize); err != nil {
		return fmt.Errorf("failed to initialize stream pool: %w", err)
	}

	deps.streamPool = GetLibp2pStreamPool()
	deps.initialized = true

	log.Info("Service initialization complete with sequencer ID: ", deps.sequencerID.String())
	return nil
}
