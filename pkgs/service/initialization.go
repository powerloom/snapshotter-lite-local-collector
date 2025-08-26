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
	
	// CRITICAL: More aggressive mesh maintenance parameters
	gossipParams.D = 10                    // Higher target mesh degree (default 6)
	gossipParams.Dlo = 8                   // Higher lower bound (default 5)
	gossipParams.Dhi = 16                  // Higher upper bound (default 12)
	gossipParams.Dlazy = 10                // Higher lazy push threshold (default 6)
	gossipParams.Dout = 4                  // Outbound connections per topic (default 2)
	gossipParams.Dscore = 6                // Score-based peer selection (default 4)
	
	// CRITICAL: Faster heartbeat for active presence
	gossipParams.HeartbeatInterval = 700 * time.Millisecond  // Much faster (default 1s)
	
	// Increase history windows to keep messages longer
	gossipParams.HistoryLength = 12        // Keep more message IDs (default 5)
	gossipParams.HistoryGossip = 6         // Gossip more history (default 3)
	
	// Configure fanout parameters for publishing
	gossipParams.FanoutTTL = 300 * time.Second  // Much longer fanout retention (default 60s)
	
	// CRITICAL: Opportunistic grafting for maintaining connections
	gossipParams.OpportunisticGraftTicks = 30     // Check every 30 heartbeats (default 60)
	gossipParams.OpportunisticGraftPeers = 4      // Graft up to 4 peers (default 2)
	
	// CRITICAL: Prune backoff to prevent re-pruning
	gossipParams.PruneBackoff = 5 * time.Minute   // Longer backoff (default 1 min)
	gossipParams.UnsubscribeBackoff = 30 * time.Second  // Unsubscribe backoff
	
	// CRITICAL: Connection manager parameters
	gossipParams.MaxPendingConnections = 256      // More pending connections
	gossipParams.ConnectionTimeout = 60 * time.Second  // Longer timeout
	
	// Configure custom peer scoring to be EXTREMELY lenient for publishers
	peerScoreParams := &pubsub.PeerScoreParams{
		// CRITICAL: Very slow decay to maintain scores
		DecayInterval: 60 * time.Second,      // Much slower decay (default 1s)
		DecayToZero:   0.001,                 // Extremely slow decay to zero
		
		// CRITICAL: Very high baseline positive scores
		AppSpecificScore: func(p peer.ID) float64 {
			// Give ourselves an extremely high score
			if p == deps.hostConn.ID() {
				return 100000.0  // Extremely high self-score
			}
			// Give all other peers a very high baseline
			return 10000.0  // Very high baseline for all peers
		},
		AppSpecificWeight: 10.0,  // Higher weight for app-specific scores
		
		// CRITICAL: Disable IP colocation penalties completely
		IPColocationFactorThreshold: 1000,    // Effectively disabled
		IPColocationFactorWeight:    0.0,     // No penalty
		
		// CRITICAL: Completely disable behavior penalties
		BehaviourPenaltyWeight:    0.0,      // NO penalty (disabled)
		BehaviourPenaltyThreshold:  1000.0,  // Very high threshold
		BehaviourPenaltyDecay:      0.999,   // Extremely slow decay
		
		// Remove time in mesh penalty for new peers
		RetainScore: 30 * time.Minute,       // Keep scores longer
	}
	
	// Configure topic-specific parameters - COMPLETELY DISABLE PENALTIES
	topicScoreParams := &pubsub.TopicScoreParams{
		// CRITICAL: High topic weight for positive scores
		TopicWeight: 10.0,  // High topic importance for positive scoring
		
		// CRITICAL: Strongly reward staying in mesh
		TimeInMeshWeight:  10.0,                     // High positive weight
		TimeInMeshQuantum: 1 * time.Second,          // Fast accumulation
		TimeInMeshCap:     10000.0,                  // Very high cap
		
		// CRITICAL: Strongly reward publishing
		FirstMessageDeliveriesWeight:  100.0,        // Very high positive weight
		FirstMessageDeliveriesDecay:   0.999,        // Extremely slow decay
		FirstMessageDeliveriesCap:     10000.0,      // Very high cap
		
		// CRITICAL: COMPLETELY DISABLE mesh delivery penalties
		MeshMessageDeliveriesWeight:     0.0,        // NO PENALTY (disabled)
		MeshMessageDeliveriesDecay:      0.999,      // Extremely slow decay if ever enabled
		MeshMessageDeliveriesThreshold:  0.001,      // Extremely low threshold
		MeshMessageDeliveriesCap:        1.0,        // Minimal cap
		MeshMessageDeliveriesActivation: 24 * time.Hour,  // Never activate in practice
		MeshMessageDeliveriesWindow:     24 * time.Hour,  // Huge window
		
		// CRITICAL: Disable ALL failure penalties
		MeshFailurePenaltyWeight: 0.0,               // No penalty for mesh failures
		MeshFailurePenaltyDecay:  0.0,
		
		// Invalid messages - standard penalties
		InvalidMessageDeliveriesWeight: -100.0,
		InvalidMessageDeliveriesDecay:  0.5,
	}
	
	// Configure score thresholds to be EXTREMELY lenient
	peerScoreThresholds := &pubsub.PeerScoreThresholds{
		GossipThreshold:             -100000,  // Extremely lenient (default -10)
		PublishThreshold:            -200000,  // Extremely lenient (default -50)
		GraylistThreshold:           -500000,  // Extremely lenient (default -80)
		AcceptPXThreshold:           -10000,   // Accept peer exchange even from negative peers
		OpportunisticGraftThreshold: -1000,    // Graft even slightly negative peers
	}
	
	// Set topic score parameters for both topics
	peerScoreParams.Topics = map[string]*pubsub.TopicScoreParams{
		"/powerloom/snapshot-submissions/0":   topicScoreParams,
		"/powerloom/snapshot-submissions/all": topicScoreParams,
	}
	
	// Configure gossipsub with custom parameters and ALL critical options
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
		pubsub.WithValidateQueueSize(256),
		pubsub.WithValidateWorkers(8),
		pubsub.WithValidateThrottle(100),     // Process more messages concurrently
		
		// Publishing configuration
		pubsub.WithFloodPublish(true),        // Flood to all peers for redundancy
		
		// Peer exchange for discovery
		pubsub.WithPeerExchange(true),
		
		// CRITICAL: Additional options to prevent pruning
		pubsub.WithMaxMessageSize(10 * 1024 * 1024),  // 10MB max message size
		pubsub.WithPeerOutboundQueueSize(1024),        // Larger outbound queue
		pubsub.WithRawTracer(nil),                     // Can add tracer for debugging
		
		// Direct peers can be added here if needed
		pubsub.WithDirectPeers([]peer.AddrInfo{}),
		
		// Message signing (can be disabled for performance in trusted networks)
		pubsub.WithMessageSigning(true),
		pubsub.WithStrictSignatureVerification(false),
	)
	if err != nil {
		return fmt.Errorf("failed to create pubsub: %w", err)
	}
	
	log.Info("Initialized gossipsub with AGGRESSIVE anti-pruning parameters for publishers")
	log.Debug("Configuration: DisabledMeshDeliveryPenalty=true, HighBaselineScores=true, FrequentHeartbeat=700ms")

	// Initialize stream pool
	if err := InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize); err != nil {
		return fmt.Errorf("failed to initialize stream pool: %w", err)
	}

	deps.streamPool = GetLibp2pStreamPool()
	deps.initialized = true

	log.Info("Service initialization complete with sequencer ID: ", deps.sequencerID.String())
	return nil
}
