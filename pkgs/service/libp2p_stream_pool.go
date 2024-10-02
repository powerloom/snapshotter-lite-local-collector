package service

import (
	"context"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	log "github.com/sirupsen/logrus"
)

type streamPool struct {
	streams      chan network.Stream
	maxSize      int
	createStream func() (network.Stream, error)
	mu           sync.Mutex
	sequencerID  peer.ID
	stopChan     chan struct{}
}

func newStreamPool(maxSize int, sequencerID peer.ID, createStream func() (network.Stream, error)) *streamPool {
	pool := &streamPool{
		streams:      make(chan network.Stream, maxSize),
		maxSize:      maxSize,
		createStream: createStream,
		sequencerID:  sequencerID,
	}
	go pool.maintainPool(context.Background())
	return pool
}

func (p *streamPool) GetStream() (network.Stream, error) {
	select {
	case stream := <-p.streams:
		if stream.Conn().IsClosed() {
			stream.Close()
			return p.createNewStream()
		}
		return stream, nil
	default:
		return p.createNewStream()
	}
}

func (p *streamPool) ReturnStream(stream network.Stream) {
	if stream.Conn().IsClosed() {
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

func (p *streamPool) Stop() {
	close(p.stopChan)
}

func (p *streamPool) maintainPool(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.fillPool()
			p.cleanPool()
		}
	}
}

func (p *streamPool) fillPool() {
	p.mu.Lock()
	defer p.mu.Unlock()

	currentSize := len(p.streams)
	for i := currentSize; i < p.maxSize; i++ {
		stream, err := p.createStream()
		if err != nil {
			log.Errorf("Failed to create stream: %v", err)
			break
		}
		p.ReturnStream(stream)
	}
}

func (p *streamPool) cleanPool() {
	p.mu.Lock()
	defer p.mu.Unlock()

	validStreams := make([]network.Stream, 0, len(p.streams))
	for len(p.streams) > 0 {
		select {
		case stream := <-p.streams:
			if !stream.Conn().IsClosed() {
				validStreams = append(validStreams, stream)
			} else {
				stream.Close()
			}
		default:
			break
		}
	}

	for _, stream := range validStreams {
		p.streams <- stream
	}
}

func (p *streamPool) createNewStream() (network.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.createStream()
}
