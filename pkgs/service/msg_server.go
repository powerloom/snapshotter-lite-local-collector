package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/rcrowley/go-metrics"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
)

// server is used to implement submission.SubmissionService.
type server struct {
	pkgs.UnimplementedSubmissionServer
	streamPool *streamPool
	limiter    *rate.Limiter
}

var _ pkgs.SubmissionServer = &server{}
var mu sync.Mutex

type serverMetrics struct {
	successfulSubmissions metrics.Counter
	failedSubmissions     metrics.Counter
	queueSize             metrics.Gauge
	processingTime        metrics.Timer
}

// NewMsgServerImpl returns an implementation of the SubmissionService interface
// for the provided Keeper.
func NewMsgServerImplV2() pkgs.SubmissionServer {
	var sequencerAddr ma.Multiaddr
	var err error

	sequencer, err := fetchSequencer("https://raw.githubusercontent.com/PowerLoom/snapshotter-lite-local-collector/feat/trusted-relayers/sequencers.json", config.SettingsObj.DataMarketAddress)
	if err != nil {
		log.Debugln(err.Error())
		return nil
	}
	sequencerAddr, err = ma.NewMultiaddr(sequencer.Maddr)
	if err != nil {
		log.Debugln(err.Error())
		return nil
	}

	sequencerInfo, err := peer.AddrInfoFromP2pAddr(sequencerAddr)

	if err != nil {
		log.Errorln("Error converting MultiAddr to AddrInfo: ", err.Error())
		return nil
	}

	sequencerID := sequencerInfo.ID

	if err := rpctorelay.Connect(context.Background(), *sequencerInfo); err != nil {
		log.Debugln("Failed to connect to the Sequencer:", err)
	} else {
		log.Debugln("Successfully connected to the Sequencer: ", sequencerAddr.String())
	}

	createStream := func() (network.Stream, error) {
		var stream network.Stream
		var err error

		operation := func() error {
			stream, err = rpctorelay.NewStream(
				network.WithUseTransient(context.Background(), "collect"),
				sequencerID,
				"/collect",
			)
			return err
		}

		backoffStrategy := backoff.NewExponentialBackOff()
		backoffStrategy.MaxElapsedTime = 30 * time.Second // Adjust as needed

		err = backoff.Retry(operation, backoffStrategy)
		if err != nil {
			return nil, fmt.Errorf("failed to create stream after retries: %w", err)
		}

		return stream, nil
	}

	s := &server{
		streamPool: newStreamPool(config.SettingsObj.MaxStreamPoolSize, createStream),
		limiter:    rate.NewLimiter(rate.Limit(500), 1), // 500 submissions per second
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
	stream, err := s.streamPool.GetStream()
	if err != nil {
		return fmt.Errorf("failed to get stream: %w", err)
	}
	defer s.streamPool.ReturnStream(stream)

	_, err = stream.Write(data)
	if err != nil {
		stream.Reset()
		return fmt.Errorf("failed to write to stream: %w", err)
	}

	return nil
}
