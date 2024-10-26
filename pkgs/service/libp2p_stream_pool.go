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

type streamPool struct {
	streams      chan network.Stream
	maxSize      int
	createStream func() (network.Stream, error)
	stopChan     chan struct{}
	healthCheck  time.Duration
	sequencerID  peer.ID
	mu           sync.Mutex
}

func newStreamPool(maxSize int, createStream func() (network.Stream, error), sequencerID peer.ID) *streamPool {
	pool := &streamPool{
		streams:      make(chan network.Stream, maxSize),
		maxSize:      maxSize,
		createStream: createStream,
		stopChan:     make(chan struct{}),
		healthCheck:  time.Duration(config.SettingsObj.StreamPoolHealthCheckInterval) * time.Second,
		sequencerID:  sequencerID,
		mu:           sync.Mutex{}, // This line is optional, as Go will initialize it to its zero value automatically
	}
	go pool.maintainPool(context.Background())
	return pool
}

func (p *streamPool) GetStream() (network.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for {
		select {
		case stream := <-p.streams:
			if err := p.pingStream(stream); err != nil {
				log.Warnf("Stream ping failed, closing: %v", err)
				stream.Close()
				continue
			}
			return stream, nil
		default:
			return p.createNewStreamWithRetry()
		}
	}
}

func (p *streamPool) ReturnStream(stream network.Stream) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.pingStream(stream); err != nil {
		stream.Close()
		return
	}

	select {
	case p.streams <- stream:
		// Stream returned to pool
	default:
		// Pool is full, close the stream
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
		stream, err = p.createStream()
		if err != nil {
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

func (p *streamPool) checkSequencerConnection() {
	if RPCToRelay.Network().Connectedness(p.sequencerID) != network.Connected {
		log.Warn("Lost connection to sequencer. Attempting to reconnect...")
		if err := ConnectToSequencer(); err != nil {
			log.Errorf("Failed to reconnect to sequencer: %v", err)
		} else {
			log.Info("Successfully reconnected to sequencer")
		}
	}
}

func (p *streamPool) maintainPool(ctx context.Context) {
	ticker := time.NewTicker(p.healthCheck)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopChan:
			return
		case <-ticker.C:
			log.Info("Starting pool maintenance")
			p.checkSequencerConnection()
			p.fillPool()
			p.cleanPool()
			log.Infof("Pool maintenance complete. Current pool size: %d", len(p.streams))
		}
	}
}

func (p *streamPool) fillPool() {
	p.mu.Lock()
	defer p.mu.Unlock()

	currentSize := len(p.streams)
	for i := currentSize; i < p.maxSize; i++ {
		stream, err := p.createNewStreamWithRetry()
		if err != nil {
			log.Errorf("Failed to create stream: %v", err)
			break
		}
		p.streams <- stream
	}
}

func (p *streamPool) cleanPool() {
	p.mu.Lock()
	defer p.mu.Unlock()

	healthyStreams := make([]network.Stream, 0, len(p.streams))
	for len(p.streams) > 0 {
		select {
		case stream := <-p.streams:
			if p.pingStream(stream) == nil {
				healthyStreams = append(healthyStreams, stream)
			} else {
				stream.Close()
			}
		default:
			// Remove the break statement
		}
	}

	for _, stream := range healthyStreams {
		p.streams <- stream
	}
}

func (p *streamPool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	close(p.stopChan)
	for len(p.streams) > 0 {
		stream := <-p.streams
		stream.Close()
	}
}
