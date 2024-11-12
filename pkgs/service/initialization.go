package service

import (
	"fmt"
	"proto-snapshot-server/config"
	"sync"
	log "github.com/sirupsen/logrus"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

type ServiceDependencies struct {
	hostConn    host.Host
	sequencerId peer.ID
	streamPool  *StreamPool
	initialized bool
	mu          sync.RWMutex
}

var (
	deps ServiceDependencies
)

func InitializeService() error {
	deps.mu.Lock()
	defer deps.mu.Unlock()

	if deps.initialized {
		log.Warn("Service already initialized")
		return nil
	}

	// Establish sequencer connection
	if err := EstablishSequencerConnection(); err != nil {
		return fmt.Errorf("failed to establish sequencer connection: %w", err)
	}

	// Verify connection state
	if SequencerHostConn == nil {
		return fmt.Errorf("sequencer host connection not initialized")
	}
	if SequencerId.String() == "" {
		return fmt.Errorf("sequencer ID not initialized")
	}

	deps.hostConn = SequencerHostConn
	deps.sequencerId = SequencerId

	// Initialize stream pool
	if err := InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize); err != nil {
		return fmt.Errorf("failed to initialize stream pool: %w", err)
	}

	deps.streamPool = GetLibp2pStreamPool()
	deps.initialized = true
	
	log.Info("Service initialization complete with sequencer ID: ", deps.sequencerId.String())
	return nil
}

