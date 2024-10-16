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
	streamPool *streamPool
}

var _ pkgs.SubmissionServer = &server{}
var mu sync.Mutex

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
	return &server{
		streamPool: newStreamPool(1024, sequencerID, createStream), // Adjust pool size as needed
	}
}

func (s *server) writeToStream(data []byte) error {
	for attempt := 0; attempt < 3; attempt++ {
		stream, err := s.streamPool.GetStream()
		if err != nil {
			log.Warnf("Failed to get stream (attempt %d): %v", attempt+1, err)
			time.Sleep(time.Second * time.Duration(attempt+1))
			continue
		}

		_, err = stream.Write(data)
		if err == nil {
			s.streamPool.ReturnStream(stream)
			return nil // Success
		}

		log.Warnf("Failed to write to stream (attempt %d): %v", attempt+1, err)
		stream.Reset() // Reset the stream instead of returning it to the pool
		time.Sleep(time.Second * time.Duration(attempt+1))
	}

	return fmt.Errorf("failed to write to stream after 3 attempts")
}

func (s *server) SubmitSnapshot(stream pkgs.Submission_SubmitSnapshotServer) error {
	log.Debugln("SubmitSnapshot called")

	for {
		submission, err := stream.Recv()
		if err != nil {
			switch {
			case err == io.EOF:
				log.Debugln("EOF reached")
			case strings.Contains(err.Error(), "context canceled"):
				log.Errorln("Stream ended by client: ")
			default:
				log.Errorln("Unexpected stream error: ", err.Error())
				return stream.Send(&pkgs.SubmissionResponse{Message: "Failure"})
			}
			return stream.Send(&pkgs.SubmissionResponse{Message: "Success"})
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
		if submission.Request.EpochId == 0 {
			log.Debugln("✅ Simulation snapshot submitted successfully to sequencer:", submissionId.String())
		} else {
			log.Debugln("✅ Snapshot submitted successfully to sequencer: ", submissionId.String())
		}
		return stream.Send(&pkgs.SubmissionResponse{Message: "Success: " + submissionId.String()})
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
