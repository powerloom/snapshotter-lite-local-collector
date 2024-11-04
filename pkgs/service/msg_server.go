package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"time"

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
		limiter:        rate.NewLimiter(rate.Limit(300), 50),
		writeSemaphore: make(chan struct{}, config.SettingsObj.MaxConcurrentWrites),
	}
	return s
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
	if config.SettingsObj.DataMarketInRequest {
		submissionBytes = append(submissionBytes, []byte(config.SettingsObj.DataMarketAddress)...)
	}
	submissionBytes = append(submissionBytes, subBytes...)

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
			log.Errorf("❌ Failed defer for submission (Project: %s, Epoch: %d) with ID: %s: %v",
				submission.Request.ProjectId, submission.Request.EpochId, submissionId, err)
			stream.Reset()
			stream.Close()
		} else {
			log.Infof("⏰ Succesful defer for submission (Project: %s, Epoch: %d) with ID: %s",
				submission.Request.ProjectId, submission.Request.EpochId, submissionId)
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
	log.Infof("✅ Successfully wrote to stream for submission (Project: %s, Epoch: %d) with ID: %s",
		submission.Request.ProjectId, submission.Request.EpochId, submissionId)
	return nil
}
