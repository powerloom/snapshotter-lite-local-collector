package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	log "github.com/sirupsen/logrus"
)

// IsResourceLimitExceeded attempts to create a regular stream to check if we're hitting resource limits
func IsResourceLimitExceeded() bool {
	ctx := context.Background()
	stream, err := SequencerHostConn.NewStream(ctx, SequencerId, "/collect")
	if err != nil {
		return strings.Contains(err.Error(), "resource limit exceeded")
	}
	// Make sure to close the test stream
	defer stream.Close()
	return false
}

// ClearExistingConnections closes all existing streams and connections
// to free up resources before attempting reconnection
func ClearExistingConnections() {
	// Close all streams in the pool first
	pool := GetLibp2pStreamPool()
	if pool != nil {
		log.Info("Clearing stream pool")
		pool.Stop()
	}

	// Close existing connection to sequencer
	if SequencerHostConn != nil {
		log.Info("Closing existing sequencer connection")

		// Close all active streams first
		for _, conn := range SequencerHostConn.Network().Conns() {
			streams := conn.GetStreams()
			for _, stream := range streams {
				if err := stream.Reset(); err != nil {
					log.Warnf("Error resetting stream: %v", err)
				}
			}
		}

		// Remove all connection handlers
		SequencerHostConn.RemoveStreamHandler("/collect")

		// Close all network connections
		if err := SequencerHostConn.Network().Close(); err != nil {
			log.Warnf("Error closing network: %v", err)
		}

		// Finally close the host
		if err := SequencerHostConn.Close(); err != nil {
			log.Warnf("Error closing sequencer connection: %v", err)
		}
	}

	// Give some time for resources to be freed
	time.Sleep(2 * time.Second)
}

// AtomicConnectionReset performs a complete reset of host, connections, and streams
func AtomicConnectionReset() error {
	log.Info("Performing atomic connection reset...")

	// Use mutex to ensure thread safety during reset
	libp2pStreamPoolMu.Lock()
	defer libp2pStreamPoolMu.Unlock()

	// Clear all existing connections and streams
	ClearExistingConnections()

	// Reconfigure the relayer to get fresh host connection
	ConfigureRelayer()

	// Attempt reconnection with fresh state
	if err := ConnectToSequencer(); err != nil {
		return fmt.Errorf("failed to reconnect after atomic reset: %w", err)
	}

	// Reinitialize the stream pool with the new host connection
	createStream := func() (network.Stream, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return SequencerHostConn.NewStream(ctx, SequencerId, "/collect")
	}
	InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize, createStream, SequencerId)

	log.Info("Atomic connection reset completed successfully")
	return nil
}
