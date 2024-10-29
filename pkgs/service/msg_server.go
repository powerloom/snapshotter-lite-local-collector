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
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/rcrowley/go-metrics"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
)

// server is used to implement submission.SubmissionService.
type server struct {
	pkgs.UnimplementedSubmissionServer
	limiter *rate.Limiter
}

var _ pkgs.SubmissionServer = &server{}

type serverMetrics struct {
	successfulSubmissions metrics.Counter
	failedSubmissions     metrics.Counter
	queueSize             metrics.Gauge
	processingTime        metrics.Timer
}

// NewMsgServerImpl returns an implementation of the SubmissionService interface
// for the provided Keeper.
func NewMsgServerImplV2() pkgs.SubmissionServer {
	createStream := func() (network.Stream, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return SequencerHostConn.NewStream(ctx, SequencerId, "/collect")
	}

	// Initialize the global stream pool if not already initialized
	if GetLibp2pStreamPool() == nil {
		InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize, createStream, SequencerId)
	}

	s := &server{
		limiter: rate.NewLimiter(rate.Limit(300), 50),
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
	go func() {
		err := s.writeToStream(submissionBytes)

		if err != nil {
			log.Errorf("❌ Failed to process submission: %v", err)
		} else {
			log.Infof("✅ Successfully processed submission with ID: %s", submissionId)
		}
	}()

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
	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		stream, err := GetLibp2pStreamPool().GetStream()
		if err != nil {
			log.Warnf("Failed to get stream (attempt %d/%d): %v", i+1, maxRetries, err)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		n, err := stream.Write(data)
		if err != nil {
			log.Warnf("Failed to write to stream (attempt %d/%d): %v", i+1, maxRetries, err)
			stream.Reset()
			GetLibp2pStreamPool().RemoveStream(stream)
			time.Sleep(time.Second * time.Duration(i+1))
			continue
		}

		// Add length verification
		if n != len(data) {
			log.Warnf("Incomplete write: wrote %d of %d bytes", n, len(data))
			stream.Reset()
			GetLibp2pStreamPool().RemoveStream(stream)
			continue
		}

		GetLibp2pStreamPool().ReturnStream(stream)
		return nil
	}

	return fmt.Errorf("failed to write to stream after %d attempts", maxRetries)
}
