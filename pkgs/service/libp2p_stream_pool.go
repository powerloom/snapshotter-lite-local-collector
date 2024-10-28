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

type streamPool struct {
	mu           sync.Mutex
	streams      []network.Stream
	maxSize      int
	createStream func() (network.Stream, error)
	sequencerID  peer.ID
	ctx          context.Context
	cancel       context.CancelFunc
}

func newStreamPool(maxSize int, createStream func() (network.Stream, error), sequencerID peer.ID) *streamPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &streamPool{
		streams:      make([]network.Stream, 0, maxSize),
		maxSize:      maxSize,
		createStream: createStream,
		sequencerID:  sequencerID,
		ctx:          ctx,
		cancel:       cancel,
	}
}

func (p *streamPool) GetStream() (network.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.streams) > 0 {
		stream := p.streams[len(p.streams)-1]
		p.streams = p.streams[:len(p.streams)-1]
		if err := p.pingStream(stream); err == nil {
			return stream, nil
		}
		// If ping fails, close the stream and create a new one
		stream.Close()
	}
	return p.createNewStreamWithRetry()
}

func (p *streamPool) ReturnStream(stream network.Stream) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.pingStream(stream); err != nil {
		stream.Close()
		return
	}

	if len(p.streams) < p.maxSize {
		p.streams = append(p.streams, stream)
	} else {
		stream.Close()
	}
}

func (p *streamPool) pingStream(stream network.Stream) error {
	if err := stream.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		return err
	}
	defer stream.SetDeadline(time.Time{})

	_, err := stream.Write([]byte("ping"))
	return err
}

func (p *streamPool) createNewStreamWithRetry() (network.Stream, error) {
	var stream network.Stream
	var err error

	operation := func() error {
		// Check connection status before attempting to create stream
		if SequencerHostConn.Network().Connectedness(p.sequencerID) != network.Connected {
			log.Warn("Connection to sequencer not established, attempting to reconnect...")
			if err := ConnectToSequencer(); err != nil {
				return fmt.Errorf("failed to reconnect to sequencer: %w", err)
			}
			log.Info("Successfully reconnected to sequencer")
		}

		stream, err = p.createStream()
		if err != nil {
			if strings.Contains(err.Error(), "resource limit exceeded") {
				log.Warn("Resource limit exceeded, forcing reconnection to sequencer...")
				if err := ConnectToSequencer(); err != nil {
					log.Errorf("Failed to reconnect to sequencer: %v", err)
				}
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

func (p *streamPool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cancel()

	for _, stream := range p.streams {
		stream.Close()
	}
	p.streams = nil
}

func (p *streamPool) RemoveStream(s network.Stream) {
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
