package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"strings"
	"sync"
	"sync/atomic"
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
	mu          sync.Mutex
	streams     []network.Stream
	maxSize     int
	sequencerID peer.ID
	metrics     struct {
		activeStreams  int
		failedWrites  int64
		successWrites int64
	}
}

// createStream is now a method of StreamPool
func (p *StreamPool) createStream() (network.Stream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.SettingsObj.StreamWriteTimeout)
	defer cancel()

	stream, err := SequencerHostConn.NewStream(ctx, p.sequencerID, "/collect")
	if err != nil {
		return nil, fmt.Errorf("new stream creation failed: %w", err)
	}

	// Set initial stream properties
	if err := stream.SetDeadline(time.Time{}); err != nil {
		stream.Reset()
		return nil, fmt.Errorf("failed to clear deadline: %w", err)
	}

	return stream, nil
}

func InitLibp2pStreamPool(maxSize int) error {
	libp2pStreamPoolMu.Lock()
	defer libp2pStreamPoolMu.Unlock()

	if libp2pStreamPool != nil {
		return fmt.Errorf("stream pool already initialized")
	}

	libp2pStreamPool = &StreamPool{
		streams:     make([]network.Stream, 0, maxSize),
		maxSize:     maxSize,
		sequencerID: deps.sequencerId,
	}

	log.Infof("Stream pool initialized with max size: %d", maxSize)
	return nil
}

func GetLibp2pStreamPool() *StreamPool {
	libp2pStreamPoolMu.RLock()
	defer libp2pStreamPoolMu.RUnlock()
	
	if libp2pStreamPool == nil {
		log.Warn("Attempted to access uninitialized stream pool")
		return nil
	}
	return libp2pStreamPool
}

func (p *StreamPool) GetStream() (network.Stream, error) {
	if p == nil {
		return nil, fmt.Errorf("stream pool is nil")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// First try to get an existing stream
	if len(p.streams) > 0 {
		stream := p.streams[len(p.streams)-1]
		p.streams = p.streams[:len(p.streams)-1]
		
		if err := p.pingStream(stream); err == nil {
			p.metrics.activeStreams++
			return stream, nil
		}
		log.Debug("Existing stream failed health check, creating new one")
		stream.Close()
	}

	// Create new stream if needed
	stream, err := p.createNewStreamWithRetry()
	if err != nil {
		atomic.AddInt64(&p.metrics.failedWrites, 1)
		return nil, fmt.Errorf("failed to create new stream: %w", err)
	}

	p.metrics.activeStreams++
	return stream, nil
}

func (p *StreamPool) ReturnStream(stream network.Stream) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Basic health check before returning to pool
	if err := p.pingStream(stream); err != nil {
		log.Debug("Stream failed health check on return, closing")
		stream.Close()
		p.metrics.activeStreams--
		return
	}

	if len(p.streams) < p.maxSize {
		p.streams = append(p.streams, stream)
	} else {
		log.Debug("Pool at capacity, closing stream")
		stream.Close()
	}
	p.metrics.activeStreams--
	atomic.AddInt64(&p.metrics.successWrites, 1)
}

func (p *StreamPool) pingStream(stream network.Stream) error {
	timeout := config.SettingsObj.StreamHealthCheckTimeout
	if timeout == 0 {
		timeout = 2 * time.Second // fallback default
	}

	if err := stream.SetDeadline(time.Now().Add(timeout)); err != nil {
		log.Debugf("Failed to set stream deadline: %v", err)
		return fmt.Errorf("failed to set deadline: %w", err)
	}
	defer stream.SetDeadline(time.Time{})

	// Simple connectivity check
	if stream.Reset() != nil {
		log.Debug("Stream failed health check")
		return fmt.Errorf("stream is not alive")
	}

	log.Debug("Stream passed health check")
	return nil
}

func (p *StreamPool) createNewStreamWithRetry() (network.Stream, error) {
	var stream network.Stream
	var err error

	operation := func() error {
		// Check connection before attempting stream creation
		if SequencerHostConn.Network().Connectedness(p.sequencerID) != network.Connected {
			log.Debug("Connection not established, attempting to connect")
			if err := ConnectToSequencer(); err != nil {
				log.Warnf("Failed to connect to sequencer: %v", err)
				return fmt.Errorf("sequencer connection failed: %w", err)
			}
		}

		stream, err = p.createStream()
		if err != nil {
			if strings.Contains(err.Error(), "connection reset") || 
			   strings.Contains(err.Error(), "broken pipe") {
				log.Warn("Stream creation failed with connection error, will retry")
				return err
			}
			// For other errors, log more details
			log.Errorf("Stream creation failed: %v", err)
			return err
		}

		// Verify stream is usable
		if err := p.pingStream(stream); err != nil {
			stream.Reset()
			return fmt.Errorf("stream verification failed: %w", err)
		}

		log.Debug("Successfully created and verified new stream")
		return nil
	}

	backOff := backoff.NewExponentialBackOff()
	backOff.MaxElapsedTime = config.SettingsObj.StreamHealthCheckTimeout
	backOff.InitialInterval = 100 * time.Millisecond
	backOff.MaxInterval = 2 * time.Second

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
