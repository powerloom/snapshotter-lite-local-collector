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

	// 1. Configure Relayer (sets up host connection)
	if err := ConfigureRelayer(); err != nil {
		return fmt.Errorf("failed to configure relayer: %w", err)
	}

	// 2. Verify both connection and sequencer ID
	if SequencerHostConn == nil {
		return fmt.Errorf("sequencer host connection not initialized")
	}
	if SequencerId.String() == "" {
		return fmt.Errorf("sequencer ID not initialized")
	}

	deps.hostConn = SequencerHostConn
	deps.sequencerId = SequencerId

	log.Infof("Initialized connection to sequencer: %s", deps.sequencerId.String())

	// 3. Initialize stream pool with verified sequencer ID
	if err := InitLibp2pStreamPool(config.SettingsObj.MaxStreamPoolSize); err != nil {
		return fmt.Errorf("failed to initialize stream pool: %w", err)
	}

	// 4. Verify stream pool initialization
	pool := GetLibp2pStreamPool()
	if pool == nil {
		return fmt.Errorf("stream pool initialization failed")
	}
	if pool.sequencerID.String() == "" {
		return fmt.Errorf("stream pool initialized with empty sequencer ID")
	}

	deps.streamPool = pool
	deps.initialized = true
	
	log.Info("Service initialization complete with sequencer ID: ", deps.sequencerId.String())
	return nil
}

