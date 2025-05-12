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
	reqQueue    chan *reqSlot  // For stream acquisition with identifiers
	activeOps   sync.WaitGroup // Track active operations
}

// reqSlot represents a request queue slot with an identifier
type reqSlot struct {
	id        string
	createdAt time.Time
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
		removeHealthFile()
		return fmt.Errorf("cannot initialize pool: %w", err)
	}

	// Connection successful, create health file
	createHealthFile()

	pool := &StreamPool{
		streams:     make([]network.Stream, 0, maxSize),
		maxSize:     maxSize,
		sequencerID: seqId,
		reqQueue:    make(chan *reqSlot, config.SettingsObj.MaxStreamQueueSize),
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
	log.Debug("🎯 Attempting to acquire stream")

	// Create a new request slot with identifier
	slot := &reqSlot{
		id:        fmt.Sprintf("req-%d", time.Now().UnixNano()),
		createdAt: time.Now(),
	}

	// First check if we can queue the request
	select {
	case p.reqQueue <- slot:
		log.Debugf("✅ Acquired request queue slot [%s]", slot.id)
	default:
		log.Warn("🚫 Request queue full - backpressure applied")
		return nil, fmt.Errorf("request queue full - try again later")
	}

	log.Debug("👥 Tracking active operation")
	p.activeOps.Add(1)
	defer func() {
		p.activeOps.Done()
		log.Debug("👋 Operation completed and untracked")
	}()

	// Now wait for refresh to complete if needed
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 30 * time.Second
	b.InitialInterval = 100 * time.Millisecond

	var stream network.Stream
	attempt := 0
	err := backoff.Retry(func() error {
		attempt++
		if connectionRefreshing.Load() {
			log.Debugf("⏳ Stream acquisition waiting for refresh (attempt %d) [slot: %s]", attempt, slot.id)
			return fmt.Errorf("connection refresh in progress")
		}

		p.mu.Lock()
		defer p.mu.Unlock()

		if len(p.streams) > 0 {
			stream = p.streams[len(p.streams)-1]
			p.streams = p.streams[:len(p.streams)-1]
			log.Debugf("🔍 Retrieved stream from pool, verifying... [slot: %s, stream: %v]", slot.id, stream.ID())

			if stream.Conn() == nil || stream.Conn().IsClosed() || stream.Conn().RemotePeer() != p.sequencerID {
				log.Debugf("⚠️ Found stale stream, closing [slot: %s, stream: %v]", slot.id, stream.ID())
				stream.Close()
				return fmt.Errorf("stale stream detected")
			}

			if err := p.pingStream(stream); err != nil {
				log.Debugf("💔 Stream health check failed, closing [slot: %s, stream: %v]", slot.id, stream.ID())
				stream.Close()
				return fmt.Errorf("stream health check failed: %v", err)
			}

			log.Debugf("✨ Retrieved healthy stream from pool [slot: %s, stream: %v]", slot.id, stream.ID())
			return nil
		}

		log.Debugf("🏗️ Creating new stream [slot: %s]", slot.id)
		newStream, err := p.createNewStreamWithRetry()
		if err != nil {
			log.Debugf("❌ Failed to create new stream: %v [slot: %s]", err, slot.id)
			return fmt.Errorf("failed to create new stream: %v", err)
		}
		stream = newStream
		log.Debugf("✅ Created new stream successfully [slot: %s, stream: %v]", slot.id, stream.ID())
		return nil
	}, b)

	if err != nil {
		// Release the request queue slot on error
		<-p.reqQueue
		log.Debugf("♻️ Released request queue slot due to error [slot: %s, duration: %v]", slot.id, time.Since(slot.createdAt))
		log.Errorf("❌ Stream acquisition failed after %d attempts: %v [slot: %s]", attempt, err, slot.id)
		return nil, fmt.Errorf("failed to acquire stream after retries: %w", err)
	}

	log.Debugf("🎉 Successfully acquired stream [slot: %s, stream: %v]", slot.id, stream.ID())
	return stream, nil
}

func (p *StreamPool) ReturnStream(stream network.Stream) {
	if stream == nil {
		log.Warn("Attempted to return nil stream to pool")
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Get the request slot that's being released
	slot := <-p.reqQueue
	if slot == nil {
		log.Error("Received nil request slot during stream return")
		return
	}

	// Don't exceed pool size
	if len(p.streams) >= p.maxSize {
		log.Debugf("Stream pool is full (%d/%d), closing stream: %v [slot: %s]", len(p.streams), p.maxSize, stream.ID(), slot.id)
		if err := stream.Reset(); err != nil {
			log.Warnf("Error resetting stream: %v [slot: %s]", err, slot.id)
		}
		stream.Close()
	} else {
		// return stream to pool and let the next request acquire it and check health
		p.streams = append(p.streams, stream)
		log.Debugf("Stream returned to pool: %v (pool size: %d/%d) [slot: %s]", stream.ID(), len(p.streams), p.maxSize, slot.id)

	}

	log.Debugf("♻️ Released request queue slot [slot: %s, duration: %v]", slot.id, time.Since(slot.createdAt))
}

func (p *StreamPool) pingStream(stream network.Stream) error {
	timeout := config.SettingsObj.StreamHealthCheckTimeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}

	if err := stream.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("failed to set deadline: %w", err)
	}
	defer stream.SetDeadline(time.Time{})

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
			log.Fatal("Lost connection to sequencer - terminating service for restart")
			return fmt.Errorf("fatal: sequencer connection lost")
		}

		if hostConn.Network().Connectedness(seqId) != network.Connected {
			log.Fatal("Lost connection to sequencer - terminating service for restart")
			return fmt.Errorf("fatal: connection to sequencer lost")
		}

		// Update pool's sequencer ID to match current connection
		p.sequencerID = seqId

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

	// Get current sequencer ID
	_, seqId, err := GetSequencerConnection()
	if err != nil {
		return fmt.Errorf("cannot rebuild: failed to get current sequencer ID: %w", err)
	}

	// Close all existing streams
	libp2pStreamPool.mu.Lock()
	for _, stream := range libp2pStreamPool.streams {
		if err := stream.Close(); err != nil {
			log.Warnf("Error closing stream during rebuild: %v", err)
		}
	}

	// Reset the pool with same capacity and update sequencer ID
	maxSize := libp2pStreamPool.maxSize
	libp2pStreamPool.streams = make([]network.Stream, 0, maxSize)
	libp2pStreamPool.sequencerID = seqId // Update the sequencer ID
	libp2pStreamPool.mu.Unlock()

	log.Info("Stream pool rebuilt after reconnection")
	return nil
}
