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

	"strconv"
	"sync/atomic"

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
	streamPool       *streamPool
	limiter          *rate.Limiter
	metrics          serverMetrics
	epochSubmissions sync.Map // Map[string]int64
	duplicateCount   int64
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
		streamPool: newStreamPool(config.SettingsObj.MaxStreamPoolSize, createStream), // Correctly initialize the stream pool
		limiter:    rate.NewLimiter(rate.Limit(500), 1),                               // 500 submissions per second
		metrics: serverMetrics{
			successfulSubmissions: metrics.NewCounter(),
			failedSubmissions:     metrics.NewCounter(),
			queueSize:             metrics.NewGauge(),
			processingTime:        metrics.NewTimer(),
		},
		epochSubmissions: sync.Map{}, // Map[string]int64
		duplicateCount:   0,
	}
	go s.monitorResources()
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

	epochID := submission.Request.EpochId
	projectID := submission.Request.ProjectId
	key := fmt.Sprintf("%d:%s", epochID, projectID)

	count, _ := s.epochSubmissions.LoadOrStore(key, int64(0))
	if count.(int64) > 0 {
		atomic.AddInt64(&s.duplicateCount, 1)
		log.Warnf("Duplicate submission for epoch %d and project %s", epochID, projectID)
	}
	s.epochSubmissions.Store(key, count.(int64)+1)

	go func() {
		start := time.Now()
		err := s.writeToStream(submissionBytes)
		s.metrics.processingTime.UpdateSince(start)

		if err != nil {
			s.metrics.failedSubmissions.Inc(1)
			log.Errorf("❌ Failed to process submission: %v", err)
		} else {
			s.metrics.successfulSubmissions.Inc(1)
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

func (s *server) monitorResources() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		s.cleanupOldEpochs()

		var totalSubmissions int64
		var uniqueEpochs int
		s.epochSubmissions.Range(func(key, value interface{}) bool {
			uniqueEpochs++
			totalSubmissions += value.(int64)
			return true
		})

		log.Infof("Successful submissions: %d", s.metrics.successfulSubmissions.Count())
		log.Infof("Failed submissions: %d", s.metrics.failedSubmissions.Count())
		log.Infof("Average processing time: %.2fs", s.metrics.processingTime.Mean())
		log.Infof("Total submissions in last hour: %d", totalSubmissions)
		log.Infof("Unique epochs in last hour: %d", uniqueEpochs)
		log.Infof("Duplicate submissions: %d", atomic.LoadInt64(&s.duplicateCount))
	}
}

func (s *server) cleanupOldEpochs() {
	currentEpoch := time.Now().Unix() / 120 // Assuming 2-minute epochs
	s.epochSubmissions.Range(func(key, value interface{}) bool {
		epochID := key.(string)
		epoch, _ := strconv.ParseInt(epochID, 10, 64)
		if currentEpoch-epoch > 30 { // Keep data for the last hour (30 epochs)
			s.epochSubmissions.Delete(epochID)
		}
		return true
	})
}

func (s *server) Start() {
	// If you want to keep any startup logic here, you can
	// Otherwise, you can remove this method if it's not needed
}
