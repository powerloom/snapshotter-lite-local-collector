package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
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
	}

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

	s.writeSemaphore <- struct{}{}
	go func() {
		defer func() { <-s.writeSemaphore }()
		if err := s.writeToStream(submissionBytes, submissionId.String(), submission); err != nil {
			log.Errorf("❌ Stream write failed for %s: %v", submissionId.String(), err)
		}
	}()

	return &pkgs.SubmissionResponse{Message: "Success"}, nil
}

func (s *server) SubmitSnapshotSimulation(stream pkgs.Submission_SubmitSnapshotSimulationServer) error {
	return nil // not implemented, will remove
}

func (s *server) writeToStream(data []byte, submissionId string, submission *pkgs.SnapshotSubmission) error {
	pool := GetLibp2pStreamPool()
	if pool == nil {
		return fmt.Errorf("stream pool not available")
	}

	stream, err := pool.GetStream()
	if err != nil {
		return fmt.Errorf("failed to get stream: %w", err)
	}

	success := false
	defer func() {
		if !success {
			if submission.Request.EpochId == 0 {
				log.Errorf("❌ Failed defer for SIMULATION snapshot submission (Project: %s, Epoch: %d) with ID: %s: %v",
					submission.Request.ProjectId, submission.Request.EpochId, submissionId, err)
			} else {
				log.Errorf("❌ Failed defer for snapshot submission (Project: %s, Epoch: %d) with ID: %s: %v",
					submission.Request.ProjectId, submission.Request.EpochId, submissionId, err)
			}
			stream.Reset()
			stream.Close()
		} else {
			if submission.Request.EpochId == 0 {
				log.Infof("⏰ Succesful defer for SIMULATION snapshot submission (Project: %s, Epoch: %d) with ID: %s",
					submission.Request.ProjectId, submission.Request.EpochId, submissionId)
			} else {
				log.Infof("⏰ Succesful defer for snapshot submission (Project: %s, Epoch: %d) with ID: %s",
					submission.Request.ProjectId, submission.Request.EpochId, submissionId)
			}
			// Get metrics and increment success counter
			if metrics := s.getOrCreateEpochMetrics(submission.Request.EpochId); metrics != nil {
				metrics.succeeded.Add(1)
			}
		}
		pool.ReturnStream(stream)
	}()

	if err := stream.SetWriteDeadline(time.Now().Add(config.SettingsObj.StreamWriteTimeout)); err != nil {
		return fmt.Errorf("❌ Failed to set write deadline for submission (Project: %s, Epoch: %d) with ID: %s: %w",
			submission.Request.ProjectId, submission.Request.EpochId, submissionId, err)
	}

	n, err := stream.Write(data)
	if err != nil {
		return fmt.Errorf("❌ Write failed for submission (Project: %s, Epoch: %d) with ID: %s: %w",
			submission.Request.ProjectId, submission.Request.EpochId, submissionId, err)
	}
	if n != len(data) {
		return fmt.Errorf("❌ Incomplete write: %d/%d bytes for submission (Project: %s, Epoch: %d) with ID: %s",
			n, len(data), submission.Request.ProjectId, submission.Request.EpochId, submissionId)
	}

	success = true
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
