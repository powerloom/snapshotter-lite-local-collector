package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	log "github.com/sirupsen/logrus"
)

// Global variables for service-wide access
var (
	libp2pStreamPool   *StreamPool
	libp2pStreamPoolMu sync.RWMutex
)

// StreamPool manages a pool of libp2p network streams
type StreamPool struct {
	mu           sync.Mutex
	streams      []network.Stream
	maxSize      int
	createStream func() (network.Stream, error)
	sequencerID  peer.ID
	ctx          context.Context
	cancel       context.CancelFunc
}

func newStreamPool(maxSize int, createStream func() (network.Stream, error), sequencerID peer.ID) *StreamPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &StreamPool{
		streams:      make([]network.Stream, 0, maxSize),
		maxSize:      maxSize,
		createStream: createStream,
		sequencerID:  sequencerID,
		ctx:          ctx,
		cancel:       cancel,
	}
}

func InitLibp2pStreamPool(maxSize int, createStream func() (network.Stream, error), sequencerID peer.ID) {
	libp2pStreamPoolMu.Lock()
	defer libp2pStreamPoolMu.Unlock()
	libp2pStreamPool = newStreamPool(maxSize, createStream, sequencerID)
}

func GetLibp2pStreamPool() *StreamPool {
	libp2pStreamPoolMu.RLock()
	defer libp2pStreamPoolMu.RUnlock()
	return libp2pStreamPool
}

func (p *StreamPool) GetStream() (network.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.streams) > 0 {
		stream := p.streams[len(p.streams)-1]
		p.streams = p.streams[:len(p.streams)-1]
		
		if err := p.pingStream(stream); err == nil {
			log.Debug("Retrieved valid stream from pool")
			return stream, nil
		}
		log.Warn("Ping failed for pooled stream, closing and creating new one")
		stream.Close()
	}
	
	log.Debug("Creating new stream")
	return p.createNewStreamWithRetry()
}

func (p *StreamPool) ReturnStream(stream network.Stream) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.pingStream(stream); err != nil {
		log.Warnf("Stream failed ping check on return: %v", err)
		stream.Close()
		return
	}

	if len(p.streams) < p.maxSize {
		log.Debug("Returning valid stream to pool")
		p.streams = append(p.streams, stream)
	} else {
		log.Debug("Pool at capacity, closing stream")
		stream.Close()
	}
}

func (p *StreamPool) pingStream(stream network.Stream) error {
	if err := stream.SetDeadline(time.Now().Add(1 * time.Second)); err != nil {
		return fmt.Errorf("failed to set deadline: %w", err)
	}
	defer stream.SetDeadline(time.Time{})

	pingMsg := []byte("ping")
	n, err := stream.Write(pingMsg)
	if err != nil {
		return fmt.Errorf("failed to write ping: %w", err)
	}
	if n != len(pingMsg) {
		return fmt.Errorf("incomplete ping write: wrote %d of %d bytes", n, len(pingMsg))
	}

	if err := stream.Flush(); err != nil {
		return fmt.Errorf("failed to flush stream after ping: %w", err)
	}

	return nil
}

func (p *StreamPool) createNewStreamWithRetry() (network.Stream, error) {
	var stream network.Stream
	var err error

	operation := func() error {
		// Check connection status before attempting to create stream
		if SequencerHostConn.Network().Connectedness(p.sequencerID) != network.Connected {
			log.Warn("Connection to sequencer not established, attempting to reconnect...")
			if err := AtomicConnectionReset(); err != nil {
				return fmt.Errorf("failed to perform atomic reset: %w", err)
			}
		}

		stream, err = p.createStream()
		if err != nil {
			if strings.Contains(err.Error(), "resource limit exceeded") {
				log.Warn("Resource limit exceeded, performing atomic reset...")
				
				if err := AtomicConnectionReset(); err != nil {
					return fmt.Errorf("atomic reset failed: %w", err)
				}

				// Try creating the stream again with fresh connection
				stream, err = p.createStream()
				if err != nil {
					return fmt.Errorf("failed to create stream even after reset: %w", err)
				}
				return nil
			}
			log.Warnf("Failed to create stream: %v. Retrying...", err)
			return err
		}
		return nil
	}

	backOff := backoff.NewExponentialBackOff()
	backOff.MaxElapsedTime = 2 * time.Minute

	err = backoff.Retry(operation, backOff)
	if err != nil {
		return nil, fmt.Errorf("failed to create stream after retries: %w", err)
	}

	return stream, nil
}

// Modified stream pool cleanup to be more aggressive
func (p *StreamPool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cancel()

	// Aggressively close all streams
	for _, stream := range p.streams {
		if err := stream.Reset(); err != nil {
			log.Warnf("Error resetting stream: %v", err)
		}
		stream.Close()
	}
	p.streams = nil

	// Wait a moment for cleanup
	time.Sleep(1 * time.Second)
}

func (p *StreamPool) RemoveStream(s network.Stream) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, stream := range p.streams {
		if stream == s {
			// Remove the stream from the slice
			p.streams = append(p.streams[:i], p.streams[i+1:]...)
			// Close the stream
			s.Close()
			// Log the removal
			log.Debugf("Removed stream from pool. Current pool size: %d", len(p.streams))
			return
		}
	}

	// If we get here, the stream wasn't in the pool
	log.Warn("Attempted to remove a stream that wasn't in the pool")
	// Close the stream anyway, just in case
	s.Close()
}
