package service

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/sethvargo/go-retry"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"io"
	"net"
	"proto-snapshot-server/pkgs"
	"time"
)

// server is used to implement submission.SubmissionService.
type server struct {
	pkgs.UnimplementedSubmissionServer
	stream network.Stream
}

var _ pkgs.SubmissionServer = &server{}

// NewMsgServerImpl returns an implementation of the SubmissionService interface
// for the provided Keeper.
func NewMsgServerImpl() pkgs.SubmissionServer {
	return &server{}
}
func setNewStream(s *server) {
	// Define the backoff strategy - Fibonacci with jitter
	backoff := retry.NewFibonacci(1 * time.Second)

	backoff = retry.WithMaxRetries(5, backoff)                // Retry up to 5 times
	backoff = retry.WithJitter(500*time.Millisecond, backoff) // Add jitter

	// Define the operation to retry
	operation := func(ctx context.Context) error {
		st, err := rpctorelay.NewStream(network.WithUseTransient(ctx, "collect"), CollectorId, "/collect")
		if err != nil {
			log.Debugln("unable to establish stream: ", err.Error())
			return retry.RetryableError(err) // Mark the error as retryable
		}
		s.stream = st
		return nil
	}

	// Execute the operation with retry
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := retry.Do(ctx, backoff, operation); err != nil {
		log.Debugln("Failed to establish stream after retries: %v", err)
	} else {
		log.Debugln("Stream established successfully")
	}
}

//
//func setNewStream(s *server) {
//	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
//	defer cancel()
//	st, err := rpctorelay.NewStream(network.WithUseTransient(ctx, "collect"), CollectorId, "/collect")
//	if err != nil {
//		log.Debugln("unable to establish stream: ", err.Error())
//	}
//	s.stream = st
//}

func (s *server) SubmitSnapshot(stream pkgs.Submission_SubmitSnapshotServer) error {
	if s.stream == nil {
		setNewStream(s)
	}
	var submissionId uuid.UUID
	for {
		submission, err := stream.Recv()

		if err == io.EOF {
			log.Debugln("EOF reached")
			break
		} else if err != nil {
			log.Errorln("Grpc server crash ", err.Error())
			return err
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
			s.stream.Close()
			setNewStream(s)

			for i := 0; i < 5; i++ {
				_, err = s.stream.Write(subBytes)
				if err == nil {
					break
				} else {
					log.Errorln("Collector stream error, retrying: ", err.Error())
					s.stream.Close()
					setNewStream(s)
					time.Sleep(time.Second * 5)
				}
			}
		}
	}
	return stream.SendAndClose(&pkgs.SubmissionResponse{Message: submissionId.String()})
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
