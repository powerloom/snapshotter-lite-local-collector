package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
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
	mu          sync.Mutex
	streams     []network.Stream
	maxSize     int
	sequencerID peer.ID
}

// createStream is now a method of StreamPool
func (p *StreamPool) createStream() (network.Stream, error) {
	if SequencerHostConn == nil {
		return nil, fmt.Errorf("no sequencer connection available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.SettingsObj.StreamWriteTimeout)
	defer cancel()

	stream, err := SequencerHostConn.NewStream(ctx, p.sequencerID, "/collect")
	if err != nil {
		return nil, fmt.Errorf("new stream creation failed: %w", err)
	}

	return stream, nil
}

func InitLibp2pStreamPool(maxSize int) error {
	libp2pStreamPoolMu.Lock()
	defer libp2pStreamPoolMu.Unlock()

	// Verify connection state
	_, seqId, err := GetSequencerConnection()
	if err != nil {
		return fmt.Errorf("cannot initialize pool: %w", err)
	}

	pool := &StreamPool{
		streams:     make([]network.Stream, 0, maxSize),
		maxSize:     maxSize,
		sequencerID: seqId,
	}

	// Pre-fill the pool with streams
	for i := 0; i < maxSize; i++ {
		stream, err := pool.createNewStreamWithRetry()
		if err != nil {
			log.Errorf("Failed to create stream %d/%d: %v", i+1, maxSize, err)
			continue
		}
		pool.streams = append(pool.streams, stream)
	}

	libp2pStreamPool = pool
	log.Infof("Stream pool initialized with %d/%d streams for sequencer: %s",
		len(pool.streams), maxSize, seqId.String())
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
	p.mu.Lock()
	defer p.mu.Unlock()

	// Keep trying until we get a stream or hit an error
	for {
		// Check if we have a usable stream
		if len(p.streams) > 0 {
			stream := p.streams[len(p.streams)-1]
			p.streams = p.streams[:len(p.streams)-1]

			// Verify stream is on current connection
			if stream.Conn().ID() != SequencerHostConn.ID().String() {
				stream.Close()
				continue
			}

			// Only do ping check if the connection looks good
			if err := p.pingStream(stream); err == nil {
				return stream, nil
			}
			stream.Close()
		}

		// Create new stream if pool is empty
		stream, err := p.createNewStreamWithRetry()
		if err != nil {
			return nil, fmt.Errorf("failed to create new stream: %w", err)
		}
		return stream, nil
	}
}

func (p *StreamPool) ReturnStream(stream network.Stream) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Don't exceed pool size
	if len(p.streams) >= p.maxSize {
		log.Debugf("Stream pool is full, closing stream: %v", stream.ID())
		stream.Close()
		return
	}

	// Optional: verify stream is still healthy before returning
	if err := p.pingStream(stream); err != nil {
		log.Debugf("Stream failed health check on return, closing: %v", stream.ID())
		stream.Close()
		return
	}

	p.streams = append(p.streams, stream)
	log.Debugf("Stream returned to pool: %v", stream.ID())
	// Note: don't decrease activeCount here as stream is still in pool
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
	defer stream.SetDeadline(time.Time{}) // Clear deadline

	// Simply check if the connection is closed
	if stream.Conn() == nil || stream.Conn().IsClosed() {
		log.Debug("Stream failed health check - connection not alive")
		return fmt.Errorf("stream is not alive")
	}

	return nil
}

func (p *StreamPool) createNewStreamWithRetry() (network.Stream, error) {
	var stream network.Stream

	operation := func() error {
		// Get current connection state
		hostConn, seqId, err := GetSequencerConnection()
		if err != nil {
			return fmt.Errorf("cannot create stream: %w", err)
		}

		if hostConn.Network().Connectedness(seqId) != network.Connected {
			return fmt.Errorf("connection to sequencer lost")
		}

		stream, err = p.createStream()
		if err != nil {
			return fmt.Errorf("stream creation failed: %w", err)
		}
		return nil
	}

	backOff := backoff.NewExponentialBackOff()
	backOff.MaxElapsedTime = config.SettingsObj.StreamHealthCheckTimeout
	backOff.InitialInterval = 100 * time.Millisecond
	backOff.MaxInterval = 2 * time.Second

	err := backoff.Retry(operation, backOff)
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

func RebuildStreamPool() error {
	libp2pStreamPoolMu.Lock()
	defer libp2pStreamPoolMu.Unlock()

	if libp2pStreamPool == nil {
		return fmt.Errorf("cannot rebuild: stream pool not initialized")
	}

	// Close all existing streams
	libp2pStreamPool.mu.Lock()
	for _, stream := range libp2pStreamPool.streams {
		if err := stream.Close(); err != nil {
			log.Warnf("Error closing stream during rebuild: %v", err)
		}
	}

	// Reset the pool with same capacity
	maxSize := libp2pStreamPool.maxSize
	libp2pStreamPool.streams = make([]network.Stream, 0, maxSize)
	libp2pStreamPool.mu.Unlock()

	log.Info("Stream pool rebuilt after reconnection")
	return nil
}
