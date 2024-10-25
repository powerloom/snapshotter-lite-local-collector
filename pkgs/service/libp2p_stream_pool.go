package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/libp2p/go-libp2p/core/network"
	log "github.com/sirupsen/logrus"
)

type streamPool struct {
	streams      chan network.Stream
	maxSize      int
	createStream func() (network.Stream, error)
	mu           sync.Mutex
	stopChan     chan struct{}
	healthCheck  time.Duration
}

func newStreamPool(maxSize int, createStream func() (network.Stream, error)) *streamPool {
	pool := &streamPool{
		streams:      make(chan network.Stream, maxSize),
		maxSize:      maxSize,
		createStream: createStream,
		stopChan:     make(chan struct{}),
		healthCheck:  time.Duration(config.SettingsObj.StreamPoolHealthCheckInterval) * time.Second,
	}
	go pool.maintainPool(context.Background())
	return pool
}

func (p *streamPool) GetStream() (network.Stream, error) {
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
	// Set a short deadline for the ping
	if err := stream.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		return err
	}
	defer stream.SetDeadline(time.Time{}) // Reset the deadline

	// Send a small ping message
	_, err := stream.Write([]byte("ping"))
	return err
}

func (p *streamPool) createNewStreamWithRetry() (network.Stream, error) {
	var stream network.Stream
	var err error

	operation := func() error {
		stream, err = p.createNewStream()
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

func (p *streamPool) createNewStream() (network.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stream, err := p.createStream()
	if err != nil {
		log.Errorf("Failed to create new stream: %v", err)
		return nil, err
	}
	log.Infof("Created new stream successfully")
	return stream, nil
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
			p.fillPool()
			p.cleanPool()
			log.Infof("Pool maintenance complete. Current pool size: %d", len(p.streams))
		}
	}
}

func (p *streamPool) fillPool() {
	currentSize := len(p.streams)
	for i := currentSize; i < p.maxSize; i++ {
		stream, err := p.createNewStream()
		if err != nil {
			log.Errorf("Failed to create stream: %v", err)
			break
		}
		p.streams <- stream
	}
}

func (p *streamPool) cleanPool() {
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
	close(p.stopChan)
	for len(p.streams) > 0 {
		stream := <-p.streams
		stream.Close()
	}
}
