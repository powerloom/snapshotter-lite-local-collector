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
	mu                sync.Mutex
	streams           []network.Stream
	maxSize           int
	sequencerID       peer.ID
	reqQueue          chan *reqSlot           // For stream acquisition with identifiers
	activeOps         sync.WaitGroup          // Track active operations
	checkedOutStreams map[network.Stream]bool // Track streams currently checked out (for invalidation on connection close)
}

// streamWithSlot bundles a stream with its request slot
type streamWithSlot struct {
	stream network.Stream
	slot   *reqSlot
}

// reqSlot represents a request queue slot with an identifier
type reqSlot struct {
	id        string
	createdAt time.Time
}

// createStream is now a method of StreamPool
func (p *StreamPool) createStream() (network.Stream, error) {
	// Always use current sequencer ID from GetSequencerConnection() to avoid stale ID issues
	hostConn, seqId, err := GetSequencerConnection()
	if err != nil {
		return nil, fmt.Errorf("no sequencer connection available: %w", err)
	}

	if hostConn == nil {
		return nil, fmt.Errorf("no sequencer connection available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.SettingsObj.StreamWriteTimeout)
	defer cancel()

	stream, err := hostConn.NewStream(ctx, seqId, "/collect")
	if err != nil {
		return nil, fmt.Errorf("new stream creation failed: %w", err)
	}

	return stream, nil
}

func InitLibp2pStreamPool(maxSize int) error {
	libp2pStreamPoolMu.Lock()
	defer libp2pStreamPoolMu.Unlock()

	// Clean up any existing pool first (handles restart scenarios)
	if libp2pStreamPool != nil {
		log.Warn("Cleaning up existing stream pool before reinitialization")
		libp2pStreamPool.Stop()
		libp2pStreamPool = nil
		// Give TCP buffers time to drain
		time.Sleep(2 * time.Second)
	}

	// Verify connection state
	_, seqId, err := GetSequencerConnection()
	if err != nil {
		return fmt.Errorf("cannot initialize pool: %w", err)
	}

	pool := &StreamPool{
		streams:           make([]network.Stream, 0, maxSize),
		maxSize:           maxSize,
		sequencerID:       seqId,
		reqQueue:          make(chan *reqSlot, config.SettingsObj.MaxStreamQueueSize),
		checkedOutStreams: make(map[network.Stream]bool),
	}

	// Pre-fill the pool with streams (with staggered creation to avoid TCP buffer buildup)
	log.Infof("Pre-filling stream pool (%d streams)...", maxSize)
	created := 0
	for i := 0; i < maxSize; i++ {
		stream, err := pool.createNewStreamWithRetry()
		if err != nil {
			log.Errorf("Failed to create stream %d/%d: %v", i+1, maxSize, err)
			continue
		}
		pool.streams = append(pool.streams, stream)
		created++

		// Stagger stream creation to avoid TCP buffer buildup after restart
		// Only add delay every 10 streams to balance startup time vs buffer pressure
		if (i+1)%10 == 0 && i < maxSize-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	libp2pStreamPool = pool
	log.Infof("Stream pool initialized with %d/%d streams for sequencer: %s",
		created, maxSize, seqId.String())

	if created < maxSize {
		log.Warnf("Stream pool only filled %d/%d streams - some may be created on-demand", created, maxSize)
	}

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

func (p *StreamPool) GetStream() (*streamWithSlot, error) {
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

			if stream.Conn() == nil || stream.Conn().IsClosed() {
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
			// Track this stream as checked out
			p.checkedOutStreams[stream] = true
			return nil
		}

		log.Debugf("🏗️ Creating new stream [slot: %s]", slot.id)
		newStream, err := p.createNewStreamWithRetry()
		if err != nil {
			log.Debugf("❌ Failed to create new stream: %v [slot: %s]", err, slot.id)
			return fmt.Errorf("failed to create new stream: %v", err)
		}
		stream = newStream
		// Track this stream as checked out
		p.checkedOutStreams[stream] = true
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
	return &streamWithSlot{stream: stream, slot: slot}, nil
}

// ReleaseStream handles cleanup of both stream and slot
func (p *StreamPool) ReleaseStream(sw *streamWithSlot, failed bool) {
	if sw == nil {
		return
	}

	// Remove from checked-out tracking
	p.mu.Lock()
	delete(p.checkedOutStreams, sw.stream)
	p.mu.Unlock()

	if failed {
		// On failure, cleanup the stream
		if sw.stream != nil {
			sw.stream.Reset()
			sw.stream.Close()
		}
	} else {
		// On success, return stream to pool
		p.mu.Lock()
		if len(p.streams) >= p.maxSize {
			// Pool full, gracefully close the stream
			sw.stream.Close()
			log.Debugf("Stream gracefully closed as pool is full: %v", sw.stream.ID())
		} else {
			p.streams = append(p.streams, sw.stream)
			log.Debugf("Stream returned to pool: %v (pool size: %d/%d)", sw.stream.ID(), len(p.streams), p.maxSize)
		}
		p.mu.Unlock()
	}

	// Always release the slot
	if sw.slot != nil {
		<-p.reqQueue
		log.Debugf("♻️ Released request queue slot [slot: %s, duration: %v]", sw.slot.id, time.Since(sw.slot.createdAt))
	}
}

// InvalidateStreamsForConnection invalidates all streams on a given connection
// Called when connection closes to immediately mark streams as dead
func (p *StreamPool) InvalidateStreamsForConnection(conn network.Conn) {
	if conn == nil {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	invalidatedCount := 0
	targetPeerID := conn.RemotePeer()

	// Invalidate streams in pool - check by peer ID since Conn() may be nil after close
	for i := len(p.streams) - 1; i >= 0; i-- {
		stream := p.streams[i]
		streamConn := stream.Conn()
		if streamConn != nil && streamConn.RemotePeer() == targetPeerID {
			stream.Close()
			p.streams = append(p.streams[:i], p.streams[i+1:]...)
			invalidatedCount++
		} else if streamConn == nil {
			// Stream already has nil connection, remove it
			stream.Close()
			p.streams = append(p.streams[:i], p.streams[i+1:]...)
			invalidatedCount++
		}
	}

	// Invalidate checked-out streams (they'll be detected on next use)
	for stream := range p.checkedOutStreams {
		streamConn := stream.Conn()
		if streamConn != nil && streamConn.RemotePeer() == targetPeerID {
			stream.Reset() // Force reset to fail any pending writes
			invalidatedCount++
		} else if streamConn == nil {
			// Stream already has nil connection, reset it
			stream.Reset()
			invalidatedCount++
		}
	}

	if invalidatedCount > 0 {
		log.Warnf("Invalidated %d streams due to connection close (peer: %s)", invalidatedCount, targetPeerID)
	}
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
			return fmt.Errorf("sequencer connection lost: %w", err)
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

	err := backoff.Retry(operation, backOff)
	if err != nil {
		log.Errorf("Failed to create stream after retries: %v", err)
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

	// Verify connection state before rebuilding
	hostConn, seqId, err := GetSequencerConnection()
	if err != nil {
		return fmt.Errorf("cannot rebuild pool: sequencer connection not available: %w", err)
	}

	if hostConn.Network().Connectedness(seqId) != network.Connected {
		return fmt.Errorf("cannot rebuild pool: not connected to sequencer %s", seqId)
	}

	// Update sequencer ID in case it changed
	libp2pStreamPool.mu.Lock()
	libp2pStreamPool.sequencerID = seqId

	// Close all existing streams
	for _, stream := range libp2pStreamPool.streams {
		if err := stream.Close(); err != nil {
			log.Warnf("Error closing stream during rebuild: %v", err)
		}
	}

	// Reset the pool with same capacity
	maxSize := libp2pStreamPool.maxSize
	libp2pStreamPool.streams = make([]network.Stream, 0, maxSize)
	libp2pStreamPool.checkedOutStreams = make(map[network.Stream]bool) // Clear checked-out tracking
	libp2pStreamPool.mu.Unlock()

	// CRITICAL: Pre-fill the pool with new streams (same as InitLibp2pStreamPool)
	log.Infof("Pre-filling stream pool after rebuild (%d streams)...", maxSize)
	created := 0
	for i := 0; i < maxSize; i++ {
		stream, err := libp2pStreamPool.createNewStreamWithRetry()
		if err != nil {
			log.Errorf("Failed to create stream %d/%d during rebuild: %v", i+1, maxSize, err)
			continue
		}
		libp2pStreamPool.mu.Lock()
		libp2pStreamPool.streams = append(libp2pStreamPool.streams, stream)
		libp2pStreamPool.mu.Unlock()
		created++

		// Stagger stream creation to avoid TCP buffer buildup
		if (i+1)%10 == 0 && i < maxSize-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	log.Infof("Stream pool rebuilt with %d/%d streams for sequencer: %s", created, maxSize, seqId.String())
	if created < maxSize {
		log.Warnf("Stream pool only filled %d/%d streams during rebuild - some may be created on-demand", created, maxSize)
	}

	return nil
}
