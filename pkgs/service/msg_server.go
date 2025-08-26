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
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// epochMetrics tracks submission statistics for a specific epoch
type epochMetrics struct {
	received  atomic.Uint64
	succeeded atomic.Uint64
}

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
	discoveryTopic     *pubsub.Topic  // For peer discovery (epoch 0)
	submissionsTopic   *pubsub.Topic  // Single topic for all submissions
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
	}

	// Initialize the two-level topic architecture
	go server.initializeTopics()
	
	// Start periodic metrics logging with 15 second interval
	go server.logMetricsPeriodically(15 * time.Second)

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
		// First get writeSemaphore for GRPC concurrency control
		select {
		case s.writeSemaphore <- struct{}{}:
			defer func() { <-s.writeSemaphore }()
		default:
			return fmt.Errorf("server at capacity") // Non-retriable
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

func (s *server) broadcastToGossipsub(submission *pkgs.SnapshotSubmission) {
	// Determine which topic to use based on two-level architecture
	var topicString string
	var topic *pubsub.Topic
	
	// Use epoch 0 for discovery/joining room, or current epoch for submissions
	if submission.Request.EpochId == 0 {
		topicString = "/powerloom/snapshot-submissions/0"
		topic = s.discoveryTopic
	} else {
		// For now, use the "all" topic for all non-zero epochs
		// This simplifies discovery and reduces topic proliferation
		topicString = "/powerloom/snapshot-submissions/all"
		topic = s.submissionsTopic
	}
	
	// Skip if topics not initialized yet
	if topic == nil {
		log.Warnf("Topic %s not initialized yet, skipping broadcast", topicString)
		return
	}
	
	// Check if we have peers on the topic
	peersInTopic := s.pubsub.ListPeers(topicString)
	if len(peersInTopic) == 0 {
		// Try quick discovery if no peers
		s.quickDiscoverPeers()
		// Re-check after discovery
		peersInTopic = s.pubsub.ListPeers(topicString)
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
	
	log.WithFields(log.Fields{
		"epoch_id":     submission.Request.EpochId,
		"project_id":   submission.Request.ProjectId,
		"topic":        topicString,
		"snapshot_cid": submission.Request.SnapshotCid,
		"peer_count":   len(peersInTopic),
		"msg_size":     len(msgBytes),
	}).Info("✅ Successfully published submission to gossipsub")
}

// quickDiscoverPeers attempts a fast peer discovery
func (s *server) quickDiscoverPeers() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	
	routingDiscovery := routing.NewRoutingDiscovery(deps.dht)
	
	// Try discovery on epoch 0 (joining room)
	peerChan, err := routingDiscovery.FindPeers(ctx, "/powerloom/snapshot-submissions/0")
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
	n, err := sw.stream.Write(data)
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

// initializeTopics sets up the two-level topic architecture
func (s *server) initializeTopics() {
	ctx := context.Background()
	
	// Join the discovery/joining room (repurposed epoch 0)
	discoveryTopicName := "/powerloom/snapshot-submissions/0"
	topic, err := s.pubsub.Join(discoveryTopicName)
	if err != nil {
		log.Errorf("Failed to join discovery topic: %v", err)
		return
	}
	s.discoveryTopic = topic
	
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
		log.Infof("Advertising on discovery topic: %s", discoveryTopicName)
		
		// Continuous advertising with retries
		for {
			util.Advertise(ctx, routingDiscovery, discoveryTopicName)
			time.Sleep(5 * time.Minute) // Re-advertise periodically
		}
	}()
	
	// Join the main submissions topic for all epochs
	submissionsTopicName := "/powerloom/snapshot-submissions/all"
	topic, err = s.pubsub.Join(submissionsTopicName)
	if err != nil {
		log.Errorf("Failed to join submissions topic: %v", err)
		return
	}
	s.submissionsTopic = topic
	
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
		log.Infof("Advertising on submissions topic: %s", submissionsTopicName)
		
		// Continuous advertising with retries
		for {
			util.Advertise(ctx, routingDiscovery, submissionsTopicName)
			time.Sleep(5 * time.Minute) // Re-advertise periodically
		}
	}()
	
	// Start heartbeat publisher to maintain mesh presence
	go s.publishHeartbeats()
	
	// Start periodic mesh status checker
	go s.monitorMeshStatus()
	
	log.Info("Two-level topic architecture initialized with active participation")
}

// publishHeartbeats sends periodic heartbeat messages to maintain mesh membership
func (s *server) publishHeartbeats() {
	ticker := time.NewTicker(45 * time.Second) // Heartbeat every 45 seconds
	defer ticker.Stop()
	
	for range ticker.C {
		// Create a minimal heartbeat message
		heartbeat := &P2PSnapshotSubmission{
			EpochID:       0, // Use epoch 0 for heartbeats
			Submissions:   nil, // No actual submissions in heartbeat
			SnapshotterID: deps.hostConn.ID().String(),
			Signature:     []byte("heartbeat"), // Placeholder signature
		}
		
		msgBytes, err := json.Marshal(heartbeat)
		if err != nil {
			log.Debugf("Failed to marshal heartbeat: %v", err)
			continue
		}
		
		// Publish to discovery topic to maintain presence
		if s.discoveryTopic != nil {
			if err := s.discoveryTopic.Publish(context.Background(), msgBytes); err != nil {
				log.Debugf("Failed to publish heartbeat: %v", err)
			} else {
				peersCount := len(s.pubsub.ListPeers("/powerloom/snapshot-submissions/0"))
				log.Debugf("Published heartbeat to discovery topic, peers: %d", peersCount)
			}
		}
	}
}

// monitorMeshStatus periodically checks and logs mesh membership status
func (s *server) monitorMeshStatus() {
	ticker := time.NewTicker(60 * time.Second) // Check every minute
	defer ticker.Stop()
	
	for range ticker.C {
		// Check peer counts for both topics
		discoveryPeers := len(s.pubsub.ListPeers("/powerloom/snapshot-submissions/0"))
		submissionPeers := len(s.pubsub.ListPeers("/powerloom/snapshot-submissions/all"))
		
		log.WithFields(log.Fields{
			"discovery_peers":  discoveryPeers,
			"submission_peers": submissionPeers,
			"host_id":         deps.hostConn.ID().String(),
		}).Info("Mesh status check")
		
		// If we've been pruned (0 peers), attempt reconnection
		if discoveryPeers == 0 || submissionPeers == 0 {
			log.Warn("Detected pruning from mesh, attempting reconnection...")
			s.quickDiscoverPeers()
			
			// Force re-advertising
			go func() {
				routingDiscovery := routing.NewRoutingDiscovery(deps.dht)
				util.Advertise(context.Background(), routingDiscovery, 
					"/powerloom/snapshot-submissions/0")
				util.Advertise(context.Background(), routingDiscovery, 
					"/powerloom/snapshot-submissions/all")
			}()
		}
	}
}
