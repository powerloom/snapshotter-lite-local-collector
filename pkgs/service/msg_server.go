package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// epochMetrics tracks submission statistics for a specific epoch
type epochMetrics struct {
	received  atomic.Uint64
	succeeded atomic.Uint64
}

// MeshState represents the health state of the gossipsub mesh
type MeshState string

const (
	MeshStateHealthy  MeshState = "healthy"
	MeshStateDegraded MeshState = "degraded"
	MeshStatePruned   MeshState = "pruned"
)

// MeshHealthMetrics tracks mesh health over time
type MeshHealthMetrics struct {
	State                    MeshState
	DiscoveryPeerCount       int
	SubmissionsPeerCount     int
	TotalConnectedPeers      int
	LastStateChange          time.Time
	ConsecutiveLowPeerCounts int
	TotalPruningEvents       int
	LastPruningTime          time.Time
	Uptime                   time.Duration
	StartTime                time.Time
	// Connection state diagnostics
	ConnectionManagerLowWater                   int
	ConnectionManagerHighWater                  int
	MeshPeerIDs                                 []string // List of peer IDs in mesh
	ConnectedPeerIDs                            []string // List of all connected peer IDs
	RecentDisconnections                        int      // Count of disconnections in last minute
	RecentDisconnectionsWeInitiatedConnection   int      // Count where we initiated the CONNECTION (not who closed it)
	RecentDisconnectionsPeerInitiatedConnection int      // Count where peer initiated the CONNECTION (not who closed it)
	LastDisconnectionTime                       time.Time
	LastDisconnectionConnectionDirection        string // "we_initiated_connection" or "peer_initiated_connection" (NOT who closed it)
	PeerTagStatus                               string // Summary of peer tagging status
}

// MeshLifecycleHook is a function type for mesh lifecycle event callbacks
type MeshLifecycleHook func(event string, metrics MeshHealthMetrics)

// server is used to implement submission.SubmissionService.
type server struct {
	pkgs.UnimplementedSubmissionServer
	writeSemaphore chan struct{} // Control concurrent writes
	metrics        *sync.Map     // map[uint64]*epochMetrics
	currentEpoch   atomic.Uint64
	pubsub         *pubsub.PubSub
	joinedTopics   map[string]*pubsub.Topic
	topicsMu       sync.Mutex

	// Two-level topic architecture
	discoveryTopic   *pubsub.Topic // For peer discovery (epoch 0)
	submissionsTopic *pubsub.Topic // Single topic for all submissions

	// Mesh health monitoring
	meshMetrics        MeshHealthMetrics
	meshMetricsMu      sync.RWMutex
	meshLifecycleHooks []MeshLifecycleHook

	// Connection state tracking
	recentDisconnections []disconnectRecord // Track disconnection timestamps and direction for diagnostics
	disconnectionsMu     sync.RWMutex
}

var _ pkgs.SubmissionServer = &server{}

// NewMsgServerImpl returns an implementation of the SubmissionService interface
// for the provided Keeper.
func NewMsgServerImplV2() pkgs.SubmissionServer {
	deps.mu.RLock()
	if !deps.initialized {
		deps.mu.RUnlock()
		log.Fatal("Cannot create server: service not initialized")
	}
	deps.mu.RUnlock()

	server := &server{
		writeSemaphore: make(chan struct{}, config.SettingsObj.MaxConcurrentWrites),
		metrics:        &sync.Map{},
		pubsub:         gossiper,
		joinedTopics:   make(map[string]*pubsub.Topic),
		meshMetrics: MeshHealthMetrics{
			State:     MeshStatePruned, // Start as pruned until mesh is actually checked
			StartTime: time.Now(),
		},
		meshLifecycleHooks:   make([]MeshLifecycleHook, 0),
		recentDisconnections: make([]disconnectRecord, 0),
	}

	// Store server instance in deps for disconnection tracking
	deps.mu.Lock()
	deps.serverInstance = server
	deps.mu.Unlock()

	// Initialize the two-level topic architecture
	go server.initializeTopics()

	// Start periodic metrics logging with 15 second interval
	go server.logMetricsPeriodically(15 * time.Second)

	// Register default mesh lifecycle hook for logging
	server.RegisterMeshLifecycleHook(func(event string, metrics MeshHealthMetrics) {
		log.WithFields(log.Fields{
			"event":                event,
			"state":                metrics.State,
			"discovery_peers":      metrics.DiscoveryPeerCount,
			"submission_peers":     metrics.SubmissionsPeerCount,
			"total_connected":      metrics.TotalConnectedPeers,
			"consecutive_low":      metrics.ConsecutiveLowPeerCounts,
			"total_pruning_events": metrics.TotalPruningEvents,
			"uptime_seconds":       int(metrics.Uptime.Seconds()),
		}).Info("🔔 Mesh lifecycle event")
	})

	// Register Slack alert hook if configured
	if config.SettingsObj.SlackWebhookURL != "" {
		InitializeSlackAlerts(config.SettingsObj.SlackWebhookURL)
		if SlackAlertInstance != nil && SlackAlertInstance.enabled {
			server.RegisterMeshLifecycleHook(func(event string, metrics MeshHealthMetrics) {
				// Only send alerts for critical events (throttling handled inside SendMeshAlert)
				// Focus on state transitions and critical conditions, not every periodic check
				if event == "mesh_state_transition:healthy->pruned" ||
					event == "mesh_state_transition:degraded->pruned" ||
					event == "mesh_state_transition:pruned->healthy" ||
					event == "mesh_state_transition:healthy->degraded" ||
					event == "mesh_state_transition:degraded->healthy" ||
					event == "zero_peer_publish_attempt" ||
					event == "mesh_recovered" {
					SlackAlertInstance.SendMeshAlert(event, metrics)
				}
				// For sustained bad states, only alert if consecutive low counts indicate real issue
				// (throttling will prevent spam)
				if (metrics.State == MeshStatePruned || metrics.State == MeshStateDegraded) &&
					metrics.ConsecutiveLowPeerCounts > 10 &&
					(event == "mesh_peer_count_change" || event == "mesh_recovery_attempt") {
					SlackAlertInstance.SendMeshAlert(event, metrics)
				}
			})
			log.Info("Slack mesh alerts enabled with throttling")
		}
	}

	// Initialize health check HTTP server (always initialize, default to 8080 if not set)
	healthCheckPort := config.SettingsObj.HealthCheckPort
	if healthCheckPort == "" {
		healthCheckPort = "8080" // Default port
	}
	InitializeHealthServer(server, healthCheckPort)
	log.Infof("Health check server initialized on port %s", healthCheckPort)

	return server
}

func StartSubmissionServer(server pkgs.SubmissionServer) {
	// Create a TCP listener on the specified port from the configuration
	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", config.SettingsObj.PortNumber))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// Create a new gRPC server instance
	grpcServer = grpc.NewServer()

	// Register the SubmissionServer with the gRPC server
	pkgs.RegisterSubmissionServer(grpcServer, server)
	log.Printf("Server listening at %v", listener.Addr())

	// Start serving requests
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

func (s *server) SubmitSnapshot(ctx context.Context, submission *pkgs.SnapshotSubmission) (*pkgs.SubmissionResponse, error) {
	log.Debugln("Received submission with request: ", submission.Request)

	// Broadcast to gossipsub for decentralized sequencers
	// Only broadcast if it's for current epoch or epoch 0 (discovery)
	go s.broadcastToGossipsub(submission)

	submissionId := uuid.New()
	submissionIdBytes, err := submissionId.MarshalText()
	if err != nil {
		log.Errorln("Error marshalling submissionId: ", err.Error())
		return &pkgs.SubmissionResponse{Message: "Failure"}, err
	}

	subBytes, err := json.Marshal(submission)
	if err != nil {
		log.Errorln("Could not marshal submission: ", err.Error())
		return &pkgs.SubmissionResponse{Message: "Failure"}, err
	}
	log.Debugln("Sending submission with ID: ", submissionId.String())

	submissionBytes := submissionIdBytes
	submissionBytes = append(submissionBytes, subBytes...)

	// Track received submission for this epoch
	metrics := s.getOrCreateEpochMetrics(submission.Request.EpochId)
	metrics.received.Add(1)

	// Single write attempt with backoff
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 30 * time.Second

	err = backoff.Retry(func() error {
		// Acquire writeSemaphore with timeout to prevent indefinite blocking
		// This allows requests to wait for capacity instead of immediately failing
		// The timeout prevents requests from blocking indefinitely when the system is overloaded
		ctx, cancel := context.WithTimeout(context.Background(), config.SettingsObj.WriteSemaphoreTimeout)
		defer cancel()

		select {
		case s.writeSemaphore <- struct{}{}:
			// Successfully acquired semaphore, defer release
			defer func() { <-s.writeSemaphore }()
		case <-ctx.Done():
			// Timeout waiting for semaphore - retriable error
			// This allows the backoff mechanism to retry after a delay
			return fmt.Errorf("timeout waiting for write capacity")
		}

		// Then try to write
		if err := s.writeToStream(submissionBytes, submissionId.String(), submission); err != nil {
			if strings.Contains(err.Error(), "request queue full") ||
				strings.Contains(err.Error(), "connection refresh in progress") {
				return err // Retriable
			}
			return backoff.Permanent(err)
		}
		metrics.succeeded.Add(1)
		return nil
	}, b)

	if err != nil {
		log.Errorf("❌ Failed to submit snapshot after retries: %v", err)
		return &pkgs.SubmissionResponse{Message: "Failure"}, err
	}

	return &pkgs.SubmissionResponse{Message: "Success"}, nil
}

func (s *server) SubmitSnapshotSimulation(stream pkgs.Submission_SubmitSnapshotSimulationServer) error {
	return nil // not implemented, will remove
}

func (s *server) writeToStream(data []byte, submissionId string, submission *pkgs.SnapshotSubmission) error {
	log.Debugf("📝 Starting stream write for submission %s", submissionId)

	pool := GetLibp2pStreamPool()
	if pool == nil {
		return fmt.Errorf("❌ stream pool not available")
	}

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 30 * time.Second

	var sw *streamWithSlot
	attempt := 0
	err := backoff.Retry(func() error {
		attempt++
		log.Debugf("🔄 Attempting to get stream (attempt %d)", attempt)
		s, err := pool.GetStream()
		if err != nil {
			if strings.Contains(err.Error(), "connection refresh in progress") {
				log.Debugf("⏳ Waiting for connection refresh to complete (attempt %d)", attempt)
				return err
			}
			log.Debugf("❌ Non-retriable error getting stream: %v", err)
			return backoff.Permanent(err)
		}
		sw = s
		log.Debug("✅ Successfully acquired stream")
		return nil
	}, b)

	if err != nil {
		return err
	}

	// Set write deadline before attempting write
	if err := sw.stream.SetWriteDeadline(time.Now().Add(config.SettingsObj.StreamWriteTimeout)); err != nil {
		// First cleanup stream, then release slot
		pool.ReleaseStream(sw, true)
		return fmt.Errorf("❌ Failed to set write deadline for submission (Project: %s, Epoch: %d) with ID: %s: %w",
			submission.Request.ProjectId, submission.Request.EpochId, submissionId, err)
	}

	// Attempt the write
	log.WithFields(log.Fields{
		"submissionID": submissionId,
		"streamID":     sw.stream.ID(),
	}).Trace("Attempting stream write")
	n, err := sw.stream.Write(data)
	log.WithFields(log.Fields{
		"submissionID": submissionId,
		"streamID":     sw.stream.ID(),
		"bytesWritten": n,
		"error":        err,
	}).Trace("Stream write completed")
	if err != nil {
		// First cleanup stream, then release slot
		pool.ReleaseStream(sw, true)
		return fmt.Errorf("❌ Write failed for submission (Project: %s, Epoch: %d) with ID: %s: %w",
			submission.Request.ProjectId, submission.Request.EpochId, submissionId, err)
	}

	if n != len(data) {
		// First cleanup stream, then release slot
		pool.ReleaseStream(sw, true)
		return fmt.Errorf("❌ Incomplete write: %d/%d bytes for submission (Project: %s, Epoch: %d) with ID: %s",
			n, len(data), submission.Request.ProjectId, submission.Request.EpochId, submissionId)
	}

	// Return stream to pool and release slot
	pool.ReleaseStream(sw, false)

	if submission.Request.EpochId == 0 {
		log.Infof("✅ Successfully wrote to stream for SIMULATION snapshot submission (Project: %s, Epoch: %d) with ID: %s",
			submission.Request.ProjectId, submission.Request.EpochId, submissionId)
	} else {
		log.Infof("✅ Successfully wrote to stream for snapshot submission (Project: %s, Epoch: %d) with ID: %s",
			submission.Request.ProjectId, submission.Request.EpochId, submissionId)
	}
	return nil
}

func (s *server) getOrCreateEpochMetrics(epochID uint64) *epochMetrics {
	// Store current epoch
	s.currentEpoch.Store(epochID)

	// Get or create metrics for this epoch
	metricsValue, _ := s.metrics.LoadOrStore(epochID, &epochMetrics{
		received:  atomic.Uint64{},
		succeeded: atomic.Uint64{},
	})

	// Type assert and return the metrics object
	metrics := metricsValue.(*epochMetrics)

	// Cleanup old epochs
	s.metrics.Range(func(key, value interface{}) bool {
		epoch := key.(uint64)
		if epochID-epoch > 3 {
			s.metrics.Delete(epoch)
		}
		return true
	})

	return metrics
}

func (s *server) GetMetrics() map[uint64]struct {
	Received  uint64
	Succeeded uint64
} {
	result := make(map[uint64]struct {
		Received  uint64
		Succeeded uint64
	})

	s.metrics.Range(func(key, value interface{}) bool {
		epochID := key.(uint64)
		metrics := value.(*epochMetrics)
		result[epochID] = struct {
			Received  uint64
			Succeeded uint64
		}{
			Received:  metrics.received.Load(),
			Succeeded: metrics.succeeded.Load(),
		}
		return true
	})

	return result
}

func (s *server) logMetricsPeriodically(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		currentEpoch := s.currentEpoch.Load()
		metrics := s.GetMetrics()

		log.WithFields(log.Fields{
			"current_epoch": currentEpoch,
			"metrics":       metrics,
		}).Info("📊 Periodic metrics report")

		// Detailed per-epoch logging
		for epochID, m := range metrics {
			successRate := float64(0)
			if m.Received > 0 {
				successRate = float64(m.Succeeded) / float64(m.Received) * 100
			}

			log.WithFields(log.Fields{
				"epoch_id":     epochID,
				"received":     m.Received,
				"succeeded":    m.Succeeded,
				"success_rate": fmt.Sprintf("%.2f%%", successRate),
			}).Info("📈 Epoch metrics")
		}
	}
}

func (s *server) GracefulShutdown() {
	log.Info("Starting graceful shutdown...")

	// Wait for all ongoing writes to complete
	for i := 0; i < cap(s.writeSemaphore); i++ {
		s.writeSemaphore <- struct{}{}
	}

	// Close the write semaphore to stop accepting new writes
	close(s.writeSemaphore)

	// Stop the gRPC server gracefully
	grpcServer.GracefulStop()

	// Stop the libp2p stream pool
	if pool := GetLibp2pStreamPool(); pool != nil {
		pool.Stop()
	}

	log.Info("🧹 Graceful shutdown complete")
}

// GracefulShutdownServer initiates the graceful shutdown for the provided SubmissionServer
func GracefulShutdownServer(s pkgs.SubmissionServer) {
	if srv, ok := s.(*server); ok {
		srv.GracefulShutdown() // Trigger the graceful shutdown of the server
		return
	}

	log.Warn("Graceful shutdown is not supported for the provided server instance")
}

func (s *server) broadcastToGossipsub(submission *pkgs.SnapshotSubmission) {
	// Determine which topic to use based on two-level architecture
	var topicString string
	var topic *pubsub.Topic

	// Use epoch 0 for discovery/joining room, or current epoch for submissions
	discoveryTopic, submissionsTopic := config.SettingsObj.GetSnapshotSubmissionTopics()

	// CRITICAL: Hold lock when reading topics to prevent race with recovery/initializeTopics()
	// Recovery can re-join topics, so we need synchronization to avoid:
	// 1. Reading nil topic pointer while recovery is re-joining
	// 2. Using stale topic pointer after recovery replaces it
	s.topicsMu.Lock()
	if submission.Request.EpochId == 0 {
		topicString = discoveryTopic
		topic = s.discoveryTopic
	} else {
		// For now, use the "all" topic for all non-zero epochs
		// This simplifies discovery and reduces topic proliferation
		topicString = submissionsTopic
		topic = s.submissionsTopic
	}

	// Check if topic is nil while holding lock to prevent race with recovery
	if topic == nil {
		s.topicsMu.Unlock()
		log.Warnf("Topic %s not initialized yet, skipping broadcast (recovery may be in progress)", topicString)
		return
	}

	// Note: We release the lock here because pubsub.Topic is thread-safe to use
	// The topic pointer won't change (topics are only set if nil, never replaced)
	// But we've verified it's not nil while holding the lock
	s.topicsMu.Unlock()

	// Check if we have peers on the topic
	peersInTopic := s.pubsub.ListPeers(topicString)
	if len(peersInTopic) == 0 {
		// Try quick discovery if no peers
		log.Debugf("No peers in topic %s, attempting quick discovery...", topicString)
		s.quickDiscoverPeers()
		// Re-check after discovery
		peersInTopic = s.pubsub.ListPeers(topicString)
		if len(peersInTopic) == 0 {
			// Log diagnostic info when still no peers
			totalPeers := len(deps.hostConn.Network().Peers())
			metrics := s.GetMeshHealthMetrics()

			log.WithFields(log.Fields{
				"topic":                topicString,
				"total_connected":      totalPeers,
				"mesh_state":           metrics.State,
				"consecutive_low":      metrics.ConsecutiveLowPeerCounts,
				"total_pruning_events": metrics.TotalPruningEvents,
				"uptime_seconds":       int(metrics.Uptime.Seconds()),
				"host_id":              deps.hostConn.ID().String(),
			}).Error("🚨 CRITICAL: Publishing to gossipsub with 0 peers in topic mesh - messages will not propagate!")

			// Trigger lifecycle hook for zero-peer publish attempt
			s.triggerMeshLifecycleHook("zero_peer_publish_attempt", metrics)
		}
	}

	// Create P2P message
	p2pSubmission := &P2PSnapshotSubmission{
		EpochID:       submission.Request.EpochId,
		Submissions:   []*pkgs.SnapshotSubmission{submission},
		SnapshotterID: deps.hostConn.ID().String(),
		Signature:     nil, // TODO: Add signing
	}

	// Marshal the message
	msgBytes, err := json.Marshal(p2pSubmission)
	if err != nil {
		log.Errorf("Error marshalling P2P submission: %v", err)
		return
	}

	// Publish the message
	err = topic.Publish(context.Background(), msgBytes)
	if err != nil {
		log.WithFields(log.Fields{
			"epoch_id":     submission.Request.EpochId,
			"project_id":   submission.Request.ProjectId,
			"topic":        topicString,
			"snapshot_cid": submission.Request.SnapshotCid,
			"error":        err.Error(),
		}).Error("❌ Failed to publish submission to gossipsub topic")
		return
	}

	// Get current mesh metrics for logging
	metrics := s.GetMeshHealthMetrics()

	log.WithFields(log.Fields{
		"epoch_id":        submission.Request.EpochId,
		"project_id":      submission.Request.ProjectId,
		"topic":           topicString,
		"snapshot_cid":    submission.Request.SnapshotCid,
		"peer_count":      len(peersInTopic),
		"msg_size":        len(msgBytes),
		"mesh_state":      metrics.State,
		"total_connected": metrics.TotalConnectedPeers,
		"uptime_seconds":  int(metrics.Uptime.Seconds()),
	}).Info("✅ Successfully published submission to gossipsub")

	// Alert if peer count is low but not zero (degraded state)
	if len(peersInTopic) > 0 && len(peersInTopic) < 2 {
		log.WithFields(log.Fields{
			"peer_count": len(peersInTopic),
			"topic":      topicString,
			"mesh_state": metrics.State,
		}).Warn("⚠️ Publishing with low peer count - mesh may be degrading")
	}
}

// quickDiscoverPeers attempts a fast peer discovery
func (s *server) quickDiscoverPeers() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	routingDiscovery := routing.NewRoutingDiscovery(deps.dht)

	// Try discovery on epoch 0 (joining room)
	discoveryTopic, _ := config.SettingsObj.GetSnapshotSubmissionTopics()
	peerChan, err := routingDiscovery.FindPeers(ctx, discoveryTopic)
	if err != nil {
		log.Debugf("Quick discovery failed: %v", err)
		return
	}

	// Connect to up to 3 peers quickly
	connectedCount := 0
	for p := range peerChan {
		if p.ID == deps.hostConn.ID() {
			continue
		}

		// Try to connect
		if err := deps.hostConn.Connect(ctx, p); err == nil {
			connectedCount++
			log.Debugf("Quick connected to peer: %s", p.ID)
			if connectedCount >= 3 {
				break
			}
		}
	}
}

// reconnectToBootstrapNodes aggressively reconnects to bootstrap nodes when all connections are lost
// This mimics how Ethereum/IPFS peers maintain connectivity - they always reconnect to bootstrap nodes
func (s *server) reconnectToBootstrapNodes() {
	if deps.hostConn == nil {
		log.Warn("Cannot reconnect to bootstrap: host not initialized")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connectedCount := 0

	// Reconnect to custom bootstrap nodes if configured
	if len(config.SettingsObj.BootstrapNodeAddrs) > 0 {
		log.Infof("Reconnecting to %d custom bootstrap nodes...", len(config.SettingsObj.BootstrapNodeAddrs))
		for i, bootstrapAddr := range config.SettingsObj.BootstrapNodeAddrs {
			if bootstrapAddr == "" {
				continue
			}

			peerMA, err := ma.NewMultiaddr(bootstrapAddr)
			if err != nil {
				log.Debugf("Invalid bootstrap multiaddr %d: %v", i+1, err)
				continue
			}

			peerinfo, err := peer.AddrInfoFromP2pAddr(peerMA)
			if err != nil {
				log.Debugf("Failed to parse bootstrap peer %d: %v", i+1, err)
				continue
			}

			// Skip if already connected
			if deps.hostConn.Network().Connectedness(peerinfo.ID) == network.Connected {
				connectedCount++
				continue
			}

			// Try to reconnect
			if err := deps.hostConn.Connect(ctx, *peerinfo); err != nil {
				log.Debugf("Failed to reconnect to bootstrap node %d (%s): %v", i+1, peerinfo.ID, err)
			} else {
				connectedCount++
				log.Infof("✅ Reconnected to bootstrap node %d: %s", i+1, peerinfo.ID)
				// Tag bootstrap nodes for protection
				if ConnManager != nil {
					ConnManager.TagPeer(peerinfo.ID, "bootstrap", 200)
				}
			}
		}
	} else {
		// Fallback to default bootstrap peers
		log.Info("Reconnecting to default bootstrap peers...")
		for _, peerAddr := range dht.DefaultBootstrapPeers {
			peerinfo, err := peer.AddrInfoFromP2pAddr(peerAddr)
			if err != nil {
				continue
			}

			// Skip if already connected
			if deps.hostConn.Network().Connectedness(peerinfo.ID) == network.Connected {
				connectedCount++
				continue
			}

			// Try to reconnect
			if err := deps.hostConn.Connect(ctx, *peerinfo); err != nil {
				log.Debugf("Failed to reconnect to default bootstrap node %s: %v", peerinfo.ID, err)
			} else {
				connectedCount++
				log.Infof("✅ Reconnected to default bootstrap node: %s", peerinfo.ID)
				// Tag bootstrap nodes for protection
				if ConnManager != nil {
					ConnManager.TagPeer(peerinfo.ID, "bootstrap", 200)
				}
			}
		}
	}

	if connectedCount > 0 {
		log.Infof("✅ Bootstrap reconnection: connected to %d bootstrap nodes", connectedCount)
	} else {
		log.Warn("⚠️ Failed to reconnect to any bootstrap nodes - network may be down")
	}
}

// initializeTopics sets up the two-level topic architecture
func (s *server) initializeTopics() {
	ctx := context.Background()

	// Get configurable topic names
	discoveryTopicName, submissionsTopicName := config.SettingsObj.GetSnapshotSubmissionTopics()

	// Join the discovery/joining room (repurposed epoch 0)
	topic, err := s.pubsub.Join(discoveryTopicName)
	if err != nil {
		log.Errorf("Failed to join discovery topic: %v", err)
		return
	}
	s.topicsMu.Lock()
	s.discoveryTopic = topic
	s.topicsMu.Unlock()

	// Subscribe to discovery topic to be a proper participant
	discoverySub, err := topic.Subscribe()
	if err != nil {
		log.Errorf("Failed to subscribe to discovery topic: %v", err)
		return
	}

	// Handle discovery topic messages with active relaying
	go func() {
		for {
			msg, err := discoverySub.Next(ctx)
			if err != nil {
				log.Debugf("Error reading from discovery topic: %v", err)
				continue
			}
			// Skip our own messages
			if msg.GetFrom() == deps.hostConn.ID() {
				continue
			}

			// Process and potentially relay the message to show activity
			log.Debugf("Received message from peer %s on discovery topic", msg.GetFrom())

			// Parse the message to check if it's valid
			var p2pSubmission P2PSnapshotSubmission
			if err := json.Unmarshal(msg.Data, &p2pSubmission); err == nil {
				// Valid message - acknowledge by updating internal state
				// This shows we're actively processing messages
				log.Debugf("Processed valid submission from %s for epoch %d",
					msg.GetFrom(), p2pSubmission.EpochID)
			}
		}
	}()

	// Advertise on discovery topic for peer finding
	go func() {
		routingDiscovery := routing.NewRoutingDiscovery(deps.dht)
		log.Debugf("Advertising on discovery topic: %s", discoveryTopicName)

		// Continuous advertising with retries
		for {
			util.Advertise(ctx, routingDiscovery, discoveryTopicName)
			time.Sleep(5 * time.Minute) // Re-advertise periodically
		}
	}()

	// Join the main submissions topic for all epochs
	topic, err = s.pubsub.Join(submissionsTopicName)
	if err != nil {
		log.Errorf("Failed to join submissions topic: %v", err)
		return
	}
	s.topicsMu.Lock()
	s.submissionsTopic = topic
	s.topicsMu.Unlock()

	// Subscribe to the topic to be a proper gossipsub participant
	submissionsSub, err := topic.Subscribe()
	if err != nil {
		log.Errorf("Failed to subscribe to submissions topic: %v", err)
		return
	}

	// Handle incoming messages with active processing
	go func() {
		messageCount := uint64(0)
		for {
			msg, err := submissionsSub.Next(ctx)
			if err != nil {
				log.Debugf("Error reading from submissions topic: %v", err)
				continue
			}
			// Skip our own messages
			if msg.GetFrom() == deps.hostConn.ID() {
				continue
			}

			messageCount++

			// Process the message actively
			var p2pSubmission P2PSnapshotSubmission
			if err := json.Unmarshal(msg.Data, &p2pSubmission); err == nil {
				log.Debugf("Processed submission #%d from peer %s for epoch %d",
					messageCount, msg.GetFrom(), p2pSubmission.EpochID)

				// Track that we've seen this message (helps with gossip)
				// The act of successfully parsing shows participation
			}
		}
	}()

	// Also advertise on the submissions topic
	go func() {
		routingDiscovery := routing.NewRoutingDiscovery(deps.dht)
		log.Debugf("Advertising on submissions topic: %s", submissionsTopicName)

		// Continuous advertising with retries
		for {
			util.Advertise(ctx, routingDiscovery, submissionsTopicName)
			time.Sleep(5 * time.Minute) // Re-advertise periodically
		}
	}()

	// Discover peers on topic names (in addition to rendezvous point)
	go func() {
		log.Debug("Starting topic-based peer discovery")
		routingDiscovery := routing.NewRoutingDiscovery(deps.dht)
		log.Debug("Topic-based peer discovery started - will run every 30 seconds")

		// Discover peers on both topics periodically
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Discover peers on discovery topic
				log.Debugf("🔍 Discovering peers on topic: %s", discoveryTopicName)
				peerChan, err := routingDiscovery.FindPeers(ctx, discoveryTopicName)
				if err != nil {
					log.Warnf("Error discovering peers on topic %s: %v", discoveryTopicName, err)
				} else {
					foundCount := 0
					connectedCount := 0
					for p := range peerChan {
						foundCount++
						if p.ID != deps.hostConn.ID() && deps.hostConn.Network().Connectedness(p.ID) != network.Connected {
							if err := deps.hostConn.Connect(ctx, p); err == nil {
								connectedCount++
								log.Debugf("✅ Connected to peer via topic discovery (%s): %s", discoveryTopicName, p.ID)
								// Protect topic-discovered peers from being pruned
								if ConnManager != nil {
									ConnManager.TagPeer(p.ID, "topic-peer", 50) // Medium priority
								}
							}
						}
					}
					if foundCount > 0 {
						meshPeers := len(s.pubsub.ListPeers(discoveryTopicName))
						log.Debugf("Topic discovery (%s): found %d peers, %d connected, %d in mesh",
							discoveryTopicName, foundCount, connectedCount, meshPeers)
					}
				}

				// Discover peers on submissions topic
				log.Debugf("🔍 Discovering peers on topic: %s", submissionsTopicName)
				peerChan, err = routingDiscovery.FindPeers(ctx, submissionsTopicName)
				if err != nil {
					log.Warnf("Error discovering peers on topic %s: %v", submissionsTopicName, err)
				} else {
					foundCount := 0
					connectedCount := 0
					for p := range peerChan {
						foundCount++
						if p.ID != deps.hostConn.ID() && deps.hostConn.Network().Connectedness(p.ID) != network.Connected {
							if err := deps.hostConn.Connect(ctx, p); err == nil {
								connectedCount++
								log.Debugf("✅ Connected to peer via topic discovery (%s): %s", submissionsTopicName, p.ID)
								// Protect topic-discovered peers from being pruned
								if ConnManager != nil {
									ConnManager.TagPeer(p.ID, "topic-peer", 50) // Medium priority
								}
							}
						}
					}
					if foundCount > 0 {
						meshPeers := len(s.pubsub.ListPeers(submissionsTopicName))
						log.Debugf("Topic discovery (%s): found %d peers, %d connected, %d in mesh",
							submissionsTopicName, foundCount, connectedCount, meshPeers)
					}
				}
			}
		}
	}()

	// Start DSV rendezvous point discovery for better peer finding
	go s.startDSVRendezvousDiscovery()

	// Start heartbeat publisher to maintain mesh presence
	go s.publishHeartbeats()

	// Start periodic mesh status checker
	go s.monitorMeshStatus()

	log.Info("Two-level topic architecture initialized with active participation and DSV rendezvous discovery")
}

// startDSVRendezvousDiscovery implements rendezvous point discovery for finding DSV nodes
func (s *server) startDSVRendezvousDiscovery() {
	ctx := context.Background()

	// Use the DSV rendezvous point for discovery
	dsvRendezvousPoint := config.SettingsObj.DSVRendezvousPoint
	if dsvRendezvousPoint == "" {
		log.Warn("DSV_RENDEZVOUS_POINT not configured, skipping DSV rendezvous discovery")
		return
	}

	log.Debugf("Starting DSV rendezvous point discovery on: %s", dsvRendezvousPoint)

	routingDiscovery := routing.NewRoutingDiscovery(deps.dht)

	// Advertise our presence on the DSV rendezvous point
	go func() {
		log.Debugf("Advertising on DSV rendezvous point: %s", dsvRendezvousPoint)

		// Initial advertising
		util.Advertise(ctx, routingDiscovery, dsvRendezvousPoint)

		// Continuous advertising with retries
		ticker := time.NewTicker(5 * time.Minute) // Re-advertise every 5 minutes
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				util.Advertise(ctx, routingDiscovery, dsvRendezvousPoint)
				log.Debugf("Re-advertised on DSV rendezvous point: %s", dsvRendezvousPoint)
			}
		}
	}()

	// Continuously discover peers from the DSV rendezvous point
	go func() {
		ticker := time.NewTicker(30 * time.Second) // Discover peers every 30 seconds
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.discoverDSVPeers(ctx, routingDiscovery, dsvRendezvousPoint)
			}
		}
	}()
}

// discoverDSVPeers finds and connects to DSV nodes through the rendezvous point
func (s *server) discoverDSVPeers(ctx context.Context, routingDiscovery *routing.RoutingDiscovery, rendezvousPoint string) {
	log.Debugf("Searching for DSV peers on rendezvous point: %s", rendezvousPoint)

	peerChan, err := routingDiscovery.FindPeers(ctx, rendezvousPoint)
	if err != nil {
		log.Warnf("Error discovering DSV peers on rendezvous point %s: %v", rendezvousPoint, err)
		return
	}

	discoveredCount := 0
	connectedCount := 0
	alreadyConnectedCount := 0
	skippedCount := 0

	for p := range peerChan {
		discoveredCount++

		if p.ID == deps.hostConn.ID() {
			skippedCount++
			continue // Skip ourselves
		}

		// Check if already connected
		if deps.hostConn.Network().Connectedness(p.ID) == network.Connected {
			alreadyConnectedCount++
			continue // Already connected
		}

		// Try to connect to the discovered peer (no address filtering - let libp2p handle it)
		if err := deps.hostConn.Connect(ctx, p); err != nil {
			log.Debugf("Failed to connect to DSV peer %s: %v", p.ID, err)
		} else {
			connectedCount++
			log.Debugf("✅ Connected to DSV peer via rendezvous: %s", p.ID)

			// Protect DSV peers from being pruned by connection manager
			if ConnManager != nil {
				ConnManager.TagPeer(p.ID, "dsv-peer", 100) // High priority tag
			}

			// Limit connections per discovery round (matching lite node)
			if connectedCount >= 10 {
				log.Debugf("Reached connection limit for this discovery round")
				break
			}
		}
	}

	if discoveredCount == 0 {
		log.Debugf("No DSV peers discovered on rendezvous point %s (DHT may still be bootstrapping)", rendezvousPoint)
	} else {
		// Log discovery results at debug level to reduce log noise
		log.Debugf("DSV rendezvous discovery: found %d peers, %d already connected, %d newly connected, %d skipped (self)",
			discoveredCount, alreadyConnectedCount, connectedCount, skippedCount)
	}
}

// publishHeartbeats sends frequent messages to maintain mesh membership and help form the mesh
// Uses two different message types:
// 1. Discovery topic (/0): Lightweight heartbeat with nil submissions (recognized and skipped by DSV)
// 2. Submissions topic (/all): Test submission with empty CID (recognized as heartbeat but helps mesh formation)
func (s *server) publishHeartbeats() {
	// CRITICAL: Send messages frequently to maintain active status and help mesh formation
	ticker := time.NewTicker(10 * time.Second) // Send every 10 seconds
	defer ticker.Stop()

	messageCounter := uint64(0)

	for range ticker.C {
		messageCounter++

		discoveryTopic, submissionsTopic := config.SettingsObj.GetSnapshotSubmissionTopics()

		// Type 1: Lightweight heartbeat for discovery topic (/0)
		// Uses nil submissions so DSV node skips it immediately (line 731 in unified/main.go)
		// Hold lock when reading topic to prevent race with initializeTopics()
		s.topicsMu.Lock()
		discoveryTopicPtr := s.discoveryTopic
		s.topicsMu.Unlock()

		if discoveryTopicPtr != nil {
			discoveryHeartbeat := &P2PSnapshotSubmission{
				EpochID:       0,   // Epoch 0 for discovery topic
				Submissions:   nil, // Nil so DSV recognizes as heartbeat and skips queueing
				SnapshotterID: deps.hostConn.ID().String(),
				Signature:     []byte(fmt.Sprintf("heartbeat-discovery-%d-%d", messageCounter, time.Now().Unix())),
			}

			discoveryBytes, err := json.Marshal(discoveryHeartbeat)
			if err != nil {
				log.Debugf("Failed to marshal discovery heartbeat: %v", err)
			} else {
				if err := discoveryTopicPtr.Publish(context.Background(), discoveryBytes); err != nil {
					log.Debugf("Failed to publish heartbeat to discovery: %v", err)
				} else {
					peersCount := len(s.pubsub.ListPeers(discoveryTopic))
					if messageCounter%6 == 0 { // Log every minute
						log.Debugf("Published heartbeat to discovery topic, peers: %d", peersCount)
					}
				}
			}
		}

		// Type 2: Test submission for submissions topic (/all)
		// Uses empty SnapshotCid so DSV recognizes as heartbeat but still helps mesh formation
		// Hold lock when reading topic to prevent race with initializeTopics()
		s.topicsMu.Lock()
		submissionsTopicPtr := s.submissionsTopic
		s.topicsMu.Unlock()

		if submissionsTopicPtr != nil {
			testSubmission := &pkgs.SnapshotSubmission{
				Request: &pkgs.Request{
					EpochId:     0, // Epoch 0 but will go to submissions topic
					ProjectId:   "test:mesh-formation:local-collector",
					SnapshotCid: "", // Empty CID so DSV recognizes as heartbeat (dequeuer.go line 229)
				},
			}

			submissionsHeartbeat := &P2PSnapshotSubmission{
				EpochID:       0,                                          // Epoch 0
				Submissions:   []*pkgs.SnapshotSubmission{testSubmission}, // Non-nil but empty CID
				SnapshotterID: deps.hostConn.ID().String(),
				Signature:     []byte(fmt.Sprintf("heartbeat-submissions-%d-%d", messageCounter, time.Now().Unix())),
			}

			submissionsBytes, err := json.Marshal(submissionsHeartbeat)
			if err != nil {
				log.Debugf("Failed to marshal submissions heartbeat: %v", err)
			} else {
				if err := submissionsTopicPtr.Publish(context.Background(), submissionsBytes); err != nil {
					log.Debugf("Failed to publish test submission to submissions: %v", err)
				} else {
					peersCount := len(s.pubsub.ListPeers(submissionsTopic))
					if messageCounter%6 == 0 { // Log every minute
						log.Debugf("Published test submission to submissions topic, peers: %d", peersCount)
					}
				}
			}
		}
	}
}

// updateMeshMetrics updates mesh health metrics and triggers lifecycle hooks
func (s *server) updateMeshMetrics(discoveryPeers, submissionPeers, totalConnectedPeers int) {
	s.meshMetricsMu.Lock()
	defer s.meshMetricsMu.Unlock()

	oldState := s.meshMetrics.State
	oldDiscoveryPeers := s.meshMetrics.DiscoveryPeerCount
	oldSubmissionPeers := s.meshMetrics.SubmissionsPeerCount

	// Collect connection state diagnostics
	discoveryTopic, submissionsTopic := config.SettingsObj.GetSnapshotSubmissionTopics()
	meshPeerSet := make(map[peer.ID]bool)
	var meshPeerIDs []string
	var connectedPeerIDs []string

	if s.pubsub != nil && deps.hostConn != nil {
		// Get mesh peers
		for _, pid := range s.pubsub.ListPeers(discoveryTopic) {
			meshPeerSet[pid] = true
			meshPeerIDs = append(meshPeerIDs, pid.String())
		}
		for _, pid := range s.pubsub.ListPeers(submissionsTopic) {
			if !meshPeerSet[pid] {
				meshPeerIDs = append(meshPeerIDs, pid.String())
			}
		}

		// Get all connected peers
		for _, pid := range deps.hostConn.Network().Peers() {
			connectedPeerIDs = append(connectedPeerIDs, pid.String())
		}
	}

	// Count recent disconnections (within last minute) and track connection direction
	// NOTE: Direction indicates who INITIATED the connection, not who closed it
	s.disconnectionsMu.RLock()
	recentDisconnects := 0
	recentDisconnectsWeInitiatedConnection := 0
	recentDisconnectsPeerInitiatedConnection := 0
	var lastDisconnectTime time.Time
	var lastDisconnectConnectionDirection string
	oneMinuteAgo := time.Now().Add(-1 * time.Minute)
	for _, record := range s.recentDisconnections {
		if record.timestamp.After(oneMinuteAgo) {
			recentDisconnects++
			if record.weInitiated {
				recentDisconnectsWeInitiatedConnection++
			} else {
				recentDisconnectsPeerInitiatedConnection++
			}
			if record.timestamp.After(lastDisconnectTime) {
				lastDisconnectTime = record.timestamp
				if record.weInitiated {
					lastDisconnectConnectionDirection = "we_initiated_connection"
				} else {
					lastDisconnectConnectionDirection = "peer_initiated_connection"
				}
			}
		}
	}
	s.disconnectionsMu.RUnlock()

	// Get connection manager state
	connLowWater := 0
	connHighWater := 0
	if ConnManager != nil {
		// Connection manager doesn't expose these directly, so we'll get from config
		connLowWater = config.SettingsObj.ConnManagerLowWater
		connHighWater = config.SettingsObj.ConnManagerHighWater
		if connLowWater == 0 {
			connLowWater = 1 // Default
		}
		if connHighWater == 0 {
			connHighWater = 1000 // Default
		}
	}

	// Build peer tag status summary
	tagStatus := fmt.Sprintf("Mesh: %d tagged, Total: %d connected", len(meshPeerIDs), len(connectedPeerIDs))
	if len(meshPeerIDs) > 0 && len(connectedPeerIDs) > len(meshPeerIDs) {
		tagStatus += fmt.Sprintf(" (%d untagged)", len(connectedPeerIDs)-len(meshPeerIDs))
	}

	// Update metrics
	s.meshMetrics.DiscoveryPeerCount = discoveryPeers
	s.meshMetrics.SubmissionsPeerCount = submissionPeers
	s.meshMetrics.TotalConnectedPeers = totalConnectedPeers
	s.meshMetrics.Uptime = time.Since(s.meshMetrics.StartTime)
	s.meshMetrics.ConnectionManagerLowWater = connLowWater
	s.meshMetrics.ConnectionManagerHighWater = connHighWater
	s.meshMetrics.MeshPeerIDs = meshPeerIDs
	s.meshMetrics.ConnectedPeerIDs = connectedPeerIDs
	s.meshMetrics.RecentDisconnections = recentDisconnects
	s.meshMetrics.RecentDisconnectionsWeInitiatedConnection = recentDisconnectsWeInitiatedConnection
	s.meshMetrics.RecentDisconnectionsPeerInitiatedConnection = recentDisconnectsPeerInitiatedConnection
	s.meshMetrics.LastDisconnectionTime = lastDisconnectTime
	s.meshMetrics.LastDisconnectionConnectionDirection = lastDisconnectConnectionDirection
	s.meshMetrics.PeerTagStatus = tagStatus

	// Determine new state
	var newState MeshState
	if discoveryPeers == 0 && submissionPeers == 0 {
		newState = MeshStatePruned
		s.meshMetrics.ConsecutiveLowPeerCounts++
		if oldState != MeshStatePruned {
			s.meshMetrics.TotalPruningEvents++
			s.meshMetrics.LastPruningTime = time.Now()
		}
	} else if discoveryPeers < 2 || submissionPeers < 2 {
		newState = MeshStateDegraded
		if oldState == MeshStatePruned {
			s.meshMetrics.ConsecutiveLowPeerCounts = 0 // Reset on recovery
		}
	} else {
		newState = MeshStateHealthy
		if oldState != MeshStateHealthy {
			s.meshMetrics.ConsecutiveLowPeerCounts = 0 // Reset on recovery
		}
	}

	// Detect state transitions and trigger hooks
	if newState != oldState {
		s.meshMetrics.State = newState
		s.meshMetrics.LastStateChange = time.Now()

		event := fmt.Sprintf("mesh_state_transition:%s->%s", oldState, newState)
		s.triggerMeshLifecycleHook(event, s.meshMetrics)

		// Determine log level based on transition direction
		// Bad transitions: healthy->degraded, healthy->pruned, degraded->pruned
		// Good transitions: pruned->healthy, pruned->degraded, degraded->healthy
		isBadTransition := (oldState == MeshStateHealthy && newState != MeshStateHealthy) ||
			(oldState == MeshStateDegraded && newState == MeshStatePruned)

		logFields := log.Fields{
			"old_state":            oldState,
			"new_state":            newState,
			"discovery_peers":      discoveryPeers,
			"submission_peers":     submissionPeers,
			"total_connected":      totalConnectedPeers,
			"consecutive_low":      s.meshMetrics.ConsecutiveLowPeerCounts,
			"total_pruning_events": s.meshMetrics.TotalPruningEvents,
			"uptime_seconds":       int(s.meshMetrics.Uptime.Seconds()),
		}

		if isBadTransition {
			log.WithFields(logFields).Errorf("🚨 MESH STATE TRANSITION: %s -> %s", oldState, newState)
		} else {
			log.WithFields(logFields).Infof("✅ MESH STATE TRANSITION: %s -> %s", oldState, newState)
		}
	} else if discoveryPeers != oldDiscoveryPeers || submissionPeers != oldSubmissionPeers {
		// Peer count changed but state didn't
		event := fmt.Sprintf("mesh_peer_count_change:discovery=%d->%d,submissions=%d->%d",
			oldDiscoveryPeers, discoveryPeers, oldSubmissionPeers, submissionPeers)
		s.triggerMeshLifecycleHook(event, s.meshMetrics)

		log.WithFields(log.Fields{
			"state":            newState,
			"discovery_peers":  fmt.Sprintf("%d->%d", oldDiscoveryPeers, discoveryPeers),
			"submission_peers": fmt.Sprintf("%d->%d", oldSubmissionPeers, submissionPeers),
			"total_connected":  totalConnectedPeers,
		}).Info("📊 Mesh peer count changed")
	}

	s.meshMetrics.State = newState
}

// triggerMeshLifecycleHook calls all registered lifecycle hooks
func (s *server) triggerMeshLifecycleHook(event string, metrics MeshHealthMetrics) {
	for _, hook := range s.meshLifecycleHooks {
		go func(h MeshLifecycleHook) {
			defer func() {
				if r := recover(); r != nil {
					log.Errorf("Panic in mesh lifecycle hook: %v", r)
				}
			}()
			h(event, metrics)
		}(hook)
	}
}

// RegisterMeshLifecycleHook registers a callback function for mesh lifecycle events
func (s *server) RegisterMeshLifecycleHook(hook MeshLifecycleHook) {
	s.meshMetricsMu.Lock()
	defer s.meshMetricsMu.Unlock()
	s.meshLifecycleHooks = append(s.meshLifecycleHooks, hook)
}

// GetMeshHealthMetrics returns current mesh health metrics (thread-safe)
func (s *server) GetMeshHealthMetrics() MeshHealthMetrics {
	s.meshMetricsMu.RLock()
	defer s.meshMetricsMu.RUnlock()
	return s.meshMetrics
}

// disconnectRecord tracks a disconnection with direction
type disconnectRecord struct {
	timestamp   time.Time
	weInitiated bool
}

// recordDisconnection tracks a disconnection event for diagnostics
func (s *server) recordDisconnection(weInitiated bool) {
	s.disconnectionsMu.Lock()
	defer s.disconnectionsMu.Unlock()

	now := time.Now()
	s.recentDisconnections = append(s.recentDisconnections, disconnectRecord{
		timestamp:   now,
		weInitiated: weInitiated,
	})

	// Keep only disconnections from last 5 minutes
	fiveMinutesAgo := now.Add(-5 * time.Minute)
	validDisconnects := make([]disconnectRecord, 0)
	for _, record := range s.recentDisconnections {
		if record.timestamp.After(fiveMinutesAgo) {
			validDisconnects = append(validDisconnects, record)
		}
	}
	s.recentDisconnections = validDisconnects
}

// monitorMeshStatus periodically checks mesh membership and recovers from pruning
func (s *server) monitorMeshStatus() {
	// Do an immediate check on startup to set initial state correctly
	discoveryTopic, submissionsTopic := config.SettingsObj.GetSnapshotSubmissionTopics()
	discoveryPeers := len(s.pubsub.ListPeers(discoveryTopic))
	submissionPeers := len(s.pubsub.ListPeers(submissionsTopic))
	totalConnectedPeers := len(deps.hostConn.Network().Peers())
	s.updateMeshMetrics(discoveryPeers, submissionPeers, totalConnectedPeers)

	ticker := time.NewTicker(5 * time.Second) // Check every 5 seconds for faster state detection
	defer ticker.Stop()

	lastRecoveryAttempt := time.Now()
	lastDetailedLog := time.Now()
	lastHourMarkCheck := time.Now()

	for range ticker.C {
		// CRITICAL: Log detailed state around 1-hour mark to diagnose mass disconnect
		uptime := time.Since(s.meshMetrics.StartTime)
		if uptime >= 55*time.Minute && uptime <= 65*time.Minute {
			// Within 5 minutes of 1-hour mark - log detailed state every 30 seconds
			if time.Since(lastHourMarkCheck) >= 30*time.Second {
				allPeers := deps.hostConn.Network().Peers()
				discoveryPeers := len(s.pubsub.ListPeers(discoveryTopic))
				submissionPeers := len(s.pubsub.ListPeers(submissionsTopic))

				log.WithFields(log.Fields{
					"uptime_minutes":   int(uptime.Minutes()),
					"uptime_seconds":   int(uptime.Seconds()),
					"total_connected":  len(allPeers),
					"discovery_peers":  discoveryPeers,
					"submission_peers": submissionPeers,
					"near_1_hour_mark": true,
				}).Warn("⏰ Near 1-hour mark - monitoring for mass disconnect event")

				lastHourMarkCheck = time.Now()
			}
		}
		// Check peer counts for both topics
		discoveryTopic, submissionsTopic := config.SettingsObj.GetSnapshotSubmissionTopics()
		discoveryPeers := len(s.pubsub.ListPeers(discoveryTopic))
		submissionPeers := len(s.pubsub.ListPeers(submissionsTopic))
		totalConnectedPeers := len(deps.hostConn.Network().Peers())

		// Periodically re-tag mesh peers to prevent our connection manager from pruning them
		// Note: DSV nodes tag incoming connections on their side, so we don't need to tag all peers
		// We only tag mesh peers to protect them from our own connection manager (LowWater=1, HighWater=1000)
		if ConnManager != nil && deps.hostConn != nil {
			meshPeers := make(map[peer.ID]bool)

			// Tag mesh peers from both topics - these are critical for gossipsub functionality
			for _, peerID := range s.pubsub.ListPeers(discoveryTopic) {
				meshPeers[peerID] = true
				ConnManager.TagPeer(peerID, "topic-peer", 50) // Medium priority for mesh peers
			}
			for _, peerID := range s.pubsub.ListPeers(submissionsTopic) {
				if !meshPeers[peerID] {
					meshPeers[peerID] = true
					ConnManager.TagPeer(peerID, "topic-peer", 50) // Medium priority for mesh peers
				}
			}
		}

		// Update metrics and detect state transitions
		s.updateMeshMetrics(discoveryPeers, submissionPeers, totalConnectedPeers)

		metrics := s.GetMeshHealthMetrics()

		// Log periodically to diagnose tag refresh behavior (every 12 iterations = 1 minute)
		if ConnManager != nil && deps.hostConn != nil && metrics.ConsecutiveLowPeerCounts%12 == 0 && totalConnectedPeers > 0 {
			log.Debugf("Peer tag refresh: %d total peers, mesh peers: %d discovery + %d submissions",
				totalConnectedPeers, discoveryPeers, submissionPeers)
		}

		// CRITICAL: Detect and recover from pruning
		if discoveryPeers == 0 || submissionPeers == 0 {
			log.WithFields(log.Fields{
				"discovery_peers":      discoveryPeers,
				"submission_peers":     submissionPeers,
				"total_connected":      totalConnectedPeers,
				"consecutive_low":      metrics.ConsecutiveLowPeerCounts,
				"total_pruning_events": metrics.TotalPruningEvents,
				"last_pruning_time":    metrics.LastPruningTime,
				"uptime_seconds":       int(metrics.Uptime.Seconds()),
				"state":                metrics.State,
			}).Error("🚨 CRITICAL: Mesh pruned - no peers in gossipsub mesh!")

			// Attempt recovery if it's been at least 30 seconds since last attempt
			if time.Since(lastRecoveryAttempt) > 30*time.Second {
				log.Warn("🔄 Attempting mesh recovery...")
				lastRecoveryAttempt = time.Now()

				s.triggerMeshLifecycleHook("mesh_recovery_attempt", metrics)

				// CRITICAL: When all connections are lost, aggressively reconnect to bootstrap nodes first
				// This is how Ethereum/IPFS peers maintain connectivity - they always reconnect to bootstrap nodes
				if totalConnectedPeers == 0 {
					log.Warn("🚨 All connections lost - aggressively reconnecting to bootstrap nodes...")
					s.reconnectToBootstrapNodes()
				}

				// Quick peer discovery
				s.quickDiscoverPeers()

				// Force re-advertising on both topics
				go func() {
					routingDiscovery := routing.NewRoutingDiscovery(deps.dht)
					ctx := context.Background()

					discoveryTopic, submissionsTopic := config.SettingsObj.GetSnapshotSubmissionTopics()
					// Re-advertise on discovery topic
					util.Advertise(ctx, routingDiscovery, discoveryTopic)
					log.Debug("Re-advertised on discovery topic")

					// Re-advertise on submissions topic
					util.Advertise(ctx, routingDiscovery, submissionsTopic)
					log.Debug("Re-advertised on submissions topic")

					// Re-join topics if needed
					s.topicsMu.Lock()
					if s.discoveryTopic == nil {
						topic, err := s.pubsub.Join(discoveryTopic)
						if err == nil {
							s.discoveryTopic = topic
							log.Info("Re-joined discovery topic")
						}
					}
					if s.submissionsTopic == nil {
						topic, err := s.pubsub.Join(submissionsTopic)
						if err == nil {
							s.submissionsTopic = topic
							log.Info("Re-joined submissions topic")
						}
					}
					s.topicsMu.Unlock()
				}()
			}
		} else {
			// Reset counter if we have peers
			if metrics.ConsecutiveLowPeerCounts > 0 {
				log.WithFields(log.Fields{
					"discovery_peers":  discoveryPeers,
					"submission_peers": submissionPeers,
					"total_connected":  totalConnectedPeers,
					"uptime_seconds":   int(metrics.Uptime.Seconds()),
				}).Info("✅ Mesh recovered - peer connections restored")
				s.triggerMeshLifecycleHook("mesh_recovered", metrics)
			}

			// Detailed status log every 5 minutes
			if time.Since(lastDetailedLog) > 5*time.Minute {
				log.WithFields(log.Fields{
					"state":                metrics.State,
					"discovery_peers":      discoveryPeers,
					"submission_peers":     submissionPeers,
					"total_connected":      totalConnectedPeers,
					"total_pruning_events": metrics.TotalPruningEvents,
					"uptime_seconds":       int(metrics.Uptime.Seconds()),
					"host_id":              deps.hostConn.ID().String(),
				}).Info("📊 Mesh health status")
				lastDetailedLog = time.Now()
			}
		}
	}
}
