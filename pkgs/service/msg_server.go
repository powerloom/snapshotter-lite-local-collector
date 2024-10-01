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
	"google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/sethvargo/go-retry"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

// server is used to implement submission.SubmissionService.
type server struct {
	pkgs.UnimplementedSubmissionServer
	stream network.Stream
}

var _ pkgs.SubmissionServer = &server{}
var mu sync.Mutex

// NewMsgServerImpl returns an implementation of the SubmissionService interface
// for the provided Keeper.
func NewMsgServerImpl() pkgs.SubmissionServer {
	return &server{}
}

func (s *server) TryConnection() error {
	var sequencerAddr ma.Multiaddr
	var err error

	sequencer, err := fetchSequencer("https://raw.githubusercontent.com/PowerLoom/snapshotter-lite-local-collector/feat/trusted-relayers/sequencers.json", config.SettingsObj.DataMarketAddress)
	if err != nil {
		log.Debugln(err.Error())
	}
	sequencerAddr, err = ma.NewMultiaddr(sequencer.Maddr)
	if err != nil {
		log.Debugln(err.Error())
		return err
	}
	sequencerInfo, err := peer.AddrInfoFromP2pAddr(sequencerAddr)

	if err != nil {
		log.Errorln("Error converting MultiAddr to AddrInfo: ", err.Error())
	}

	SequencerId = sequencerInfo.ID
	operation := func() error {
		// Open a new stream to the sequencer
		stream, err := rpctorelay.NewStream(
			network.WithUseTransient(context.Background(), "collect"),
			sequencerInfo.ID,
			"/collect",
		)
		if err != nil {
			log.Debugln("Unable to establish stream: ", err.Error())
			return retry.RetryableError(err)
		}

		// Set the new stream
		s.stream = stream
		log.Debugln("Stream established successfully to sequencer: ", sequencerAddr)
		return nil
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = time.Second
	bo.MaxElapsedTime = 1 * time.Minute // Adjust as needed

	return backoff.Retry(operation, bo)
}


func (s *server) SubmitSnapshot(stream pkgs.Submission_SubmitSnapshotServer) error {
	mu.Lock()
	if s.stream == nil || s.stream.Conn().IsClosed() {
		if err := s.TryConnection(); err != nil {
			log.Errorln("Unexpected connection error: ", err.Error())
			mu.Unlock()
			return stream.Send(&pkgs.SubmissionResponse{Message: "Failure: Unable to connect to sequencer"})
		}
	}
	mu.Unlock()

	for {
		select {
		case <-stream.Context().Done():
			log.Debugln("Stream context done")
			return nil
		default:
			submission, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					log.Debugln("EOF reached")
					stream.Send(&pkgs.SubmissionResponse{Message: "Success"})
					continue
				}
				if strings.Contains(err.Error(), "context canceled") {
					log.Debugln("Stream ended by client")
					return nil
				}
				if status.Code(err) == codes.Canceled {
					log.Debugln("Stream canceled by client")
					return nil
				}
				log.Errorln("Unexpected stream error: ", err.Error())
				return nil
			}

			log.Debugln("Received submission with request: ", submission.Request)

			submissionId := uuid.New()
			submissionIdBytes, err := submissionId.MarshalText()
			if err != nil {
				log.Errorln("Error marshalling submissionId: ", err.Error())
				return stream.Send(&pkgs.SubmissionResponse{Message: "Failure"})
			}

			subBytes, err := json.Marshal(submission)
			if err != nil {
				log.Errorln("Could not marshal submission: ", err.Error())
				return stream.Send(&pkgs.SubmissionResponse{Message: "Failure"})
			}
			log.Debugln("Sending submission with ID: ", submissionId.String())

			submissionBytes := append(submissionIdBytes, subBytes...)

			err = s.writeToStream(submissionBytes)
			if err != nil {
				log.Errorln("Failed to write to stream: ", err.Error())
				return stream.Send(&pkgs.SubmissionResponse{Message: "Failure: " + submissionId.String()})
			}
			log.Debugln("Stream write successful for ID: ", submissionId.String(), "for Epoch:", submission.Request.EpochId, "Slot:", submission.Request.SlotId)
		}
	}
}

func (s *server) writeToStream(data []byte) error {
	mu.Lock()
	defer mu.Unlock()

	if s.stream == nil || s.stream.Conn().IsClosed() {
		if err := s.TryConnection(); err != nil {
			return fmt.Errorf("connection error: %w", err)
		}
	}

	return backoff.Retry(func() error {
		_, err := s.stream.Write(data)
		if err != nil {
			log.Errorln("Sequencer stream error, retrying: ", err.Error())
			s.stream.Close()
			s.stream = nil // Reset the stream so TryConnection will be called on next attempt
			return err
		}
		return nil
	}, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 2))
}

func (s *server) SubmitSnapshotSimulation(stream pkgs.Submission_SubmitSnapshotSimulationServer) error {
	mu.Lock()
	if s.stream == nil || s.stream.Conn().IsClosed() {
		if err := s.TryConnection(); err != nil {
			log.Errorln("Unexpected connection error: ", err.Error())
			mu.Unlock()
			return stream.SendAndClose(&pkgs.SubmissionResponse{Message: "Failure: Unable to connect to sequencer"})
		}
	}
	mu.Unlock()

	for {
		select {
		case <-stream.Context().Done():
			log.Debugln("Stream context done")
			return nil
		default:
			submission, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					log.Debugln("EOF reached")
					return nil
				}
				if strings.Contains(err.Error(), "context canceled") {
					log.Debugln("Stream ended by client")
					return nil
				}
				log.Errorln("Unexpected stream error: ", err.Error())
				return err
			}

			log.Debugln("Received submission with request: ", submission.Request)

			submissionId := uuid.New()
			submissionIdBytes, err := submissionId.MarshalText()
			if err != nil {
				log.Errorln("Error marshalling submissionId: ", err.Error())
				if err := stream.SendAndClose(&pkgs.SubmissionResponse{Message: "Failure"}); err != nil {
					log.Errorln("Failed to send error response: ", err.Error())
				}
				continue
			}

			subBytes, err := json.Marshal(submission)
			if err != nil {
				log.Errorln("Could not marshal submission: ", err.Error())
				if err := stream.SendAndClose(&pkgs.SubmissionResponse{Message: "Failure"}); err != nil {
					log.Errorln("Failed to send error response: ", err.Error())
				}
				continue
			}

			log.Debugln("Sending submission with ID: ", submissionId.String())

			submissionBytes := append(submissionIdBytes, subBytes...)

			err = s.writeToStream(submissionBytes)
			if err != nil {
				log.Errorln("Failed to write to stream: ", err.Error())
				if err := stream.SendAndClose(&pkgs.SubmissionResponse{Message: "Failure: " + submissionId.String()}); err != nil {
					log.Errorln("Failed to send error response: ", err.Error())
				}
				continue
			}

			log.Debugln("Stream write successful for ID: ", submissionId.String(), "for Epoch:", submission.Request.EpochId, "Slot:", submission.Request.SlotId)
			// Send success response for this submission
			if err := stream.SendAndClose(&pkgs.SubmissionResponse{Message: "Success: " + submissionId.String()}); err != nil {
				log.Errorln("Failed to send success response: ", err.Error())
				return err
			}
		}
	}
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
