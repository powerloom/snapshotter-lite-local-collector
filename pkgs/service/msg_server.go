package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
)

// server is used to implement submission.SubmissionService.
type server struct {
	pkgs.UnimplementedSubmissionServer
	limiter        *rate.Limiter
	writeSemaphore chan struct{} // Control concurrent writes
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

	s := &server{
		limiter: rate.NewLimiter(rate.Limit(300), 50),
		writeSemaphore: make(chan struct{}, config.SettingsObj.MaxConcurrentWrites),
	}
	return s
}

func (s *server) SubmitSnapshot(ctx context.Context, submission *pkgs.SnapshotSubmission) (*pkgs.SubmissionResponse, error) {
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

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

	submissionBytes := append(submissionIdBytes, subBytes...)
	if config.SettingsObj.DataMarketInRequest {
		// Convert to checksum address using go-ethereum's utility
		checksummedAddress := common.HexToAddress(config.SettingsObj.DataMarketAddress).Hex()
		submissionBytes = append([]byte(checksummedAddress), submissionBytes...)
	}

	// Launch goroutine but with controlled concurrency
	select {
	case s.writeSemaphore <- struct{}{}:
		go func() {
			defer func() { <-s.writeSemaphore }()

			err := s.writeToStream(submissionBytes)
			if err != nil {
				log.Errorf("❌ Failed to process submission %s: %v", submissionId, err)
				return
			}
			log.Infof("✅ Successfully processed submission %s", submissionId)
		}()
	default:
		// If we can't acquire semaphore immediately, log and continue
		log.Warnf("High write concurrency, submission %s queued", submissionId)
		go func() {
			s.writeSemaphore <- struct{}{} // Will block until capacity available
			defer func() { <-s.writeSemaphore }()

			err := s.writeToStream(submissionBytes)
			if err != nil {
				log.Errorf("❌ Failed to process submission %s: %v", submissionId, err)
				return
			}
			log.Infof("✅ Successfully processed submission %s", submissionId)
		}()
	}

	return &pkgs.SubmissionResponse{Message: "Success: " + submissionId.String()}, nil
}

func (s *server) SubmitSnapshotSimulation(stream pkgs.Submission_SubmitSnapshotSimulationServer) error {
	return nil // not implemented, will remove
}

func (s *server) mustEmbedUnimplementedSubmissionServer() {
}

func StartSubmissionServer(server pkgs.SubmissionServer) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", config.SettingsObj.PortNumber))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	pkgs.RegisterSubmissionServer(s, server)
	log.Printf("Server listening at %v", lis.Addr())

	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

func (s *server) writeToStream(data []byte) error {
	pool := GetLibp2pStreamPool()
	if pool == nil {
		return fmt.Errorf("stream pool not available")
	}

	if pool.sequencerID.String() == "" {
		return fmt.Errorf("invalid sequencer ID in pool")
	}

	stream, err := pool.GetStream()
	if err != nil {
		return fmt.Errorf("failed to get stream: %w", err)
	}

	maxRetries := config.SettingsObj.MaxWriteRetries
	if maxRetries == 0 {
		maxRetries = 3 // fallback default
	}

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		// Set write deadline
		writeTimeout := config.SettingsObj.StreamWriteTimeout
		if writeTimeout > 0 {
			if err := stream.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				log.Warnf("Failed to set write deadline: %v", err)
			}
		}

		n, err := stream.Write(data)

		// Clear write deadline
		stream.SetWriteDeadline(time.Time{})

		if err != nil {
			lastErr = fmt.Errorf("attempt %d: write failed: %w", i+1, err)
			log.Warnf("%v", lastErr)
			stream.Reset()
			continue
		}

		if n != len(data) {
			lastErr = fmt.Errorf("attempt %d: incomplete write: %d/%d bytes", i+1, n, len(data))
			log.Warnf("%v", lastErr)
			stream.Reset()
			continue
		}

		log.Debugf("Successfully wrote %d bytes on attempt %d", n, i+1)
		pool.ReturnStream(stream)
		return nil
	}

	return fmt.Errorf("failed after %d attempts, last error: %v", maxRetries, lastErr)
}
