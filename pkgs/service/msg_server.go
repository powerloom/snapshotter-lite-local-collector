package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/sethvargo/go-retry"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"io"
	"net"
	"proto-snapshot-server/config"
	"proto-snapshot-server/pkgs"
	"strings"
	"sync"
	"time"
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
func setNewStream(s *server) error {
	operation := func() error {
		st, err := rpctorelay.NewStream(network.WithUseTransient(context.Background(), "collect"), SequencerId, "/collect")
		if err != nil {
			log.Debugln("unable to establish stream: ", err.Error())
			ConnectToSequencer(rpctorelay.ID())
			return retry.RetryableError(err) // Mark the error as retryable
		}
		s.stream = st
		return nil
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = time.Second
	if err := backoff.Retry(operation, backoff.WithMaxRetries(bo, 2)); err != nil {
		return errors.New(fmt.Sprintf("Failed to establish stream after retries: %s", err.Error()))
	} else {
		log.Debugln("Stream established successfully")
	}
	return nil
}

// TODO: Maintain a global list of visited peers and continue connection establishment from the last accepted peer; refresh the list only when all the peers have been visited
func mustSetStream(s *server) {
	var peers []peer.ID
	var connectedPeer peer.ID
	var err error
	operation := func() error {
		err = setNewStream(s)
		if err != nil {
			log.Errorln(err.Error())
			connectedPeer = ConnectToPeer(context.Background(), routingDiscovery, config.SettingsObj.RelayerRendezvousPoint, rpctorelay, peers)
			if len(connectedPeer.String()) > 0 {
				peers = append(peers)
				ConnectToSequencer(connectedPeer)
			} else {
				return errors.New("No peer connections formed")
			}
		}
		return err
	}
	backoff.Retry(operation, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 7))
}

func TryConnection(s *server) {
	ConnectToSequencer(rpctorelay.ID())
	mustSetStream(s)
}

func (s *server) SubmitSnapshot(stream pkgs.Submission_SubmitSnapshotServer) error {
	mu.Lock()
	if s.stream == nil || s.stream.Conn().IsClosed() {
		TryConnection(s)
	}
	mu.Unlock()
	var submissionId uuid.UUID
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
				return err
			}

			return stream.Send(&pkgs.SubmissionResponse{Message: "Success"})
		}

		log.Debugln("Received submission with request: ", submission.Request)

		submissionId = uuid.New() // Generates a new UUID
		submissionIdBytes, err := submissionId.MarshalText()

		subBytes, err := json.Marshal(submission)
		if err != nil {
			log.Debugln("Error marshalling submissionId: ", err.Error())
		}
		log.Debugln("Sending submission with ID: ", submissionId.String())

		submissionBytes := append(submissionIdBytes, subBytes...)
		if err != nil {
			log.Debugln("Could not marshal submission")
			return err
		}
		if _, err = s.stream.Write(submissionBytes); err != nil {
			log.Debugln("Stream write error: ", err.Error())
			s.stream.Close()

			mu.Lock()
			TryConnection(s)
			mu.Unlock()

			backoff.Retry(func() error {
				_, err = s.stream.Write(subBytes)
				if err != nil {
					log.Errorln("Sequencer stream error, retrying: ", err.Error())
				}
				return err
			}, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 2))
		}
		stream.Send(&pkgs.SubmissionResponse{Message: "Success: " + submissionId.String()})
	}
}

func (s *server) mustEmbedUnimplementedSubmissionServer() {
}

func StartSubmissionServer(server pkgs.SubmissionServer) {
	lis, err := net.Listen("tcp", ":50051")

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
