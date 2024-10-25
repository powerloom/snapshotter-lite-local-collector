package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// server is used to implement submission.SubmissionService.
type server struct {
	pkgs.UnimplementedSubmissionServer
	streamPool      *streamPool
	submissionQueue chan *submissionJob
	processInterval time.Duration
}

var _ pkgs.SubmissionServer = &server{}
var mu sync.Mutex

type submissionJob struct {
	id   uuid.UUID
	data []byte
}

// NewMsgServerImpl returns an implementation of the SubmissionService interface
// for the provided Keeper.
func NewMsgServerImpl() pkgs.SubmissionServer {
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
		streamPool:      newStreamPool(config.SettingsObj.MaxStreamPoolSize, sequencerID, createStream),
		submissionQueue: make(chan *submissionJob, 10000), // Large buffer to handle spikes
		processInterval: 500 * time.Millisecond,          // Adjust this value to control processing rate
	}
	go s.submissionProcessor()
	return s
}

func (s *server) SubmitSnapshot(ctx context.Context, submission *pkgs.SnapshotSubmission) (*pkgs.SubmissionResponse, error) {
	log.Debugln("SubmitSnapshot called")

	submissionId := uuid.New()
	submissionIdBytes, err := submissionId.MarshalText()
	if err != nil {
		log.Errorln("Error marshalling submissionId: ", err.Error())
		return &pkgs.SubmissionResponse{Message: "Failure"}, err
	}

	dataMarketAddressBytes := []byte(config.SettingsObj.DataMarketAddress)

	subBytes, err := json.Marshal(submission)
	if err != nil {
		log.Errorln("Could not marshal submission: ", err.Error())
		return &pkgs.SubmissionResponse{Message: "Failure"}, err
	}

	submissionBytes := append(submissionIdBytes, dataMarketAddressBytes...)
	submissionBytes = append(submissionBytes, subBytes...)

	job := &submissionJob{
		id:   submissionId,
		data: submissionBytes,
	}

	select {
	case s.submissionQueue <- job:
		log.Debugf("Queued submission with ID: %s", submissionId)
		return &pkgs.SubmissionResponse{Message: "Success: " + submissionId.String()}, nil
	default:
		log.Warnf("Submission queue full, dropping submission: %s", submissionId)
		return &pkgs.SubmissionResponse{Message: "Failure: Queue full"}, fmt.Errorf("queue full")
	}
}

func (s *server) SubmitSnapshotSimulation(stream pkgs.Submission_SubmitSnapshotSimulationServer) error {
	return nil // not implemented, will remove
}

func (s *server) mustEmbedUnimplementedSubmissionServer() {
}

func StartSubmissionServer(server pkgs.SubmissionServer) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", config.SettingsObj.PortNumber))

	if err != nil {
		log.Debugf("failed to listen: %v", err)
	}

	s := grpc.NewServer()
	pkgs.RegisterSubmissionServer(s, server)
	log.Debugln("Server listening at", lis.Addr())

	if err := s.Serve(lis); err != nil {
		log.Debugf("failed to serve: %v", err)
	}
}

func (s *server) submissionProcessor() {
	ticker := time.NewTicker(s.processInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			select {
			case job := <-s.submissionQueue:
				s.processSubmission(job)
			default:
				// No job available, continue to next tick
			}
		}
	}
}

func (s *server) processSubmission(job *submissionJob) {
	maxRetries := 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := s.writeToStream(job.data)
		if err == nil {
			log.Infof("✅ Successfully processed submission with ID: %s", job.id)
			return
		}
		log.Warnf("❌ Failed to process submission %s (attempt %d): %v", job.id, attempt+1, err)
		time.Sleep(time.Duration(attempt+1) * time.Second) // Simple backoff
	}
	log.Errorf("❌ Failed to process submission %s after %d attempts", job.id, maxRetries)
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
