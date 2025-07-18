package service

import (
	"context"
	"fmt"
	"proto-snapshot-server/config"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	circuitv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	ma "github.com/multiformats/go-multiaddr"
	log "github.com/sirupsen/logrus"
)

var (
	SequencerHostConn    host.Host
	SequencerID          peer.ID
	sequencerMu          sync.RWMutex
	ConnManager          *connmgr.BasicConnMgr
	TcpAddr              ma.Multiaddr
	rm                   network.ResourceManager
	connectionRefreshing atomic.Bool
)

// Thread-safe getter for connection state
func GetSequencerConnection() (host.Host, peer.ID, error) {
	sequencerMu.RLock()
	defer sequencerMu.RUnlock()

	if SequencerHostConn == nil || SequencerID.String() == "" {
		return nil, "", fmt.Errorf("sequencer connection not established")
	}

	return SequencerHostConn, SequencerID, nil
}

func ConnectToSequencerP2P(relayers []Relayer, p2pHost host.Host) bool {
	for _, relayer := range relayers {
		relayerMA, _ := ma.NewMultiaddr(relayer.Maddr)
		relayerInfo, _ := peer.AddrInfoFromP2pAddr(relayerMA)

		if reservation, err := circuitv2.Reserve(context.Background(), p2pHost, *relayerInfo); err != nil {
			log.Fatalf("Failed to request reservation with relay: %v", err)
		} else {
			fmt.Println("Reservation with relay successful", reservation.Expiration, reservation.LimitDuration)
		}

		sequencerAddr, err := ma.NewMultiaddr(fmt.Sprintf("%s/p2p-circuit/p2p/%s", relayer.Maddr, config.SettingsObj.SequencerID))
		if err != nil {
			log.Debugln(err.Error())
		}
		log.Debugln("Connecting to Sequencer: ", sequencerAddr.String())

		isConnected := AddPeerConnection(context.Background(), p2pHost, sequencerAddr.String())
		if isConnected {
			return true
		}
	}

	return false
}

func CreateLibP2pHost() error {
	var err error
	TcpAddr, _ = ma.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%s", config.SettingsObj.LocalCollectorP2PPort))

	ConnManager, _ = connmgr.NewConnManager(
		40960,
		81920,
		connmgr.WithGracePeriod(1*time.Minute))

	scalingLimits := rcmgr.DefaultLimits

	libp2p.SetDefaultServiceLimits(&scalingLimits)

	scaledDefaultLimits := scalingLimits.AutoScale()

	cfg := rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			StreamsOutbound: rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
			Streams:         rcmgr.Unlimited,
			Conns:           rcmgr.Unlimited,
			ConnsOutbound:   rcmgr.Unlimited,
			ConnsInbound:    rcmgr.Unlimited,
			FD:              rcmgr.Unlimited,
			Memory:          rcmgr.LimitVal64(rcmgr.Unlimited),
		},
	}

	limits := cfg.Build(scaledDefaultLimits)

	limiter := rcmgr.NewFixedLimiter(limits)

	rm, err = rcmgr.NewResourceManager(limiter, rcmgr.WithMetricsDisabled())

	if err != nil {
		log.Debugln("Error instantiating resource manager: ", err.Error())
		return err
	}

	SequencerHostConn, err = libp2p.New(
		libp2p.EnableRelay(),
		libp2p.ConnectionManager(ConnManager),
		libp2p.ListenAddrs(TcpAddr),
		libp2p.ResourceManager(rm),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.Security(noise.ID, noise.New),
		libp2p.DefaultTransports,
		libp2p.NATPortMap(),
		libp2p.EnableRelayService(),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport))

	if err != nil {
		log.Debugln("Error instantiating libp2p host: ", err.Error())
		return err
	}

	// Connect to bootstrap node if configured
	if config.SettingsObj.BootstrapNodeAddr != "" {
		log.Infof("Attempting to connect to bootstrap node: %s", config.SettingsObj.BootstrapNodeAddr)
		bootstrapMA, err := ma.NewMultiaddr(config.SettingsObj.BootstrapNodeAddr)
		if err != nil {
			log.Errorf("Invalid bootstrap multiaddress: %v", err)
		} else {
			bootstrapInfo, err := peer.AddrInfoFromP2pAddr(bootstrapMA)
			if err != nil {
				log.Errorf("Failed to parse bootstrap peer info: %v", err)
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := SequencerHostConn.Connect(ctx, *bootstrapInfo); err != nil {
					log.Errorf("Failed to connect to bootstrap node %s: %v", config.SettingsObj.BootstrapNodeAddr, err)
				} else {
					log.Infof("Successfully connected to bootstrap node: %s", config.SettingsObj.BootstrapNodeAddr)
				}
			}
		}
	}

	// Connect to bootstrap node if configured
	if config.SettingsObj.BootstrapNodeAddr != "" {
		log.Infof("Attempting to connect to bootstrap node: %s", config.SettingsObj.BootstrapNodeAddr)
		bootstrapMA, err := ma.NewMultiaddr(config.SettingsObj.BootstrapNodeAddr)
		if err != nil {
			log.Errorf("Invalid bootstrap multiaddress: %v", err)
		} else {
			bootstrapInfo, err := peer.AddrInfoFromP2pAddr(bootstrapMA)
			if err != nil {
				log.Errorf("Failed to parse bootstrap peer info: %v", err)
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := SequencerHostConn.Connect(ctx, *bootstrapInfo); err != nil {
					log.Errorf("Failed to connect to bootstrap node %s: %v", config.SettingsObj.BootstrapNodeAddr, err)
				} else {
					log.Infof("Successfully connected to bootstrap node: %s", config.SettingsObj.BootstrapNodeAddr)
				}
			}
		}
	}

	log.Infof("✅ LibP2P host created. ID: %s", SequencerHostConn.ID().String())
	log.Infof("Listening on addresses: %s", SequencerHostConn.Addrs())

	return nil
}

// EstablishSequencerConnection should only be called during initialization
// or explicit reconnection logic, not during stream operations
func EstablishSequencerConnection() error {
	sequencerMu.Lock()
	defer sequencerMu.Unlock()

	// Clear existing connection if any
	if SequencerHostConn != nil {
		if err := SequencerHostConn.Close(); err != nil {
			log.Warnf("Error closing existing connection: %v", err)
		}
		// Important: Signal that connection is being reset
		// This should trigger cleanup of existing stream pool
		SequencerHostConn = nil
		SequencerID = ""
	}

	// 1. Create properly configured host
	if err := CreateLibP2pHost(); err != nil {
		return fmt.Errorf("failed to create libp2p host: %w", err)
	}

	// No need to reassign SequencerHostConn as it's already set in CreateLibP2pHost()

	// 2. Get sequencer info
	sequencer, err := fetchSequencer(
		"https://raw.githubusercontent.com/PowerLoom/snapshotter-lite-local-collector/feat/trusted-relayers/sequencers.json",
		config.SettingsObj.DataMarketAddress,
	)
	if err != nil {
		return fmt.Errorf("failed to fetch sequencer info: %w", err)
	}

	// 3. Parse multiaddr and create peer info
	maddr, err := ma.NewMultiaddr(sequencer.Maddr)
	if err != nil {
		return fmt.Errorf("failed to parse multiaddr: %w", err)
	}

	sequencerInfo, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		return fmt.Errorf("failed to get addr info: %w", err)
	}

	// 4. Set sequencer ID
	SequencerID = sequencerInfo.ID
	if SequencerID.String() == "" {
		return fmt.Errorf("empty sequencer ID")
	}

	// 5. Establish connection with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := SequencerHostConn.Connect(ctx, *sequencerInfo); err != nil {
		return fmt.Errorf("failed to connect to sequencer: %w", err)
	}

	log.Infof("Successfully connected to Sequencer: %s with ID: %s", sequencer.Maddr, SequencerID.String())
	return nil
}

func StartConnectionRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(config.SettingsObj.ConnectionRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Info("🔄 Starting periodic connection refresh cycle")

			connectionRefreshing.Store(true)
			log.Info("🚫 Connection refresh state activated - new streams will wait")

			pool := GetLibp2pStreamPool()
			if pool == nil {
				log.Error("❌ Stream pool not available for refresh")
				connectionRefreshing.Store(false)
				continue
			}

			// Wait for in-flight requests with exponential backoff
			b := backoff.NewExponentialBackOff()
			b.MaxElapsedTime = 30 * time.Second
			b.InitialInterval = 100 * time.Millisecond

			log.Info("⏳ Waiting for in-flight requests to complete")
			err := backoff.Retry(func() error {
				filled := 0
				// First try to get all slots to check for active requests
				for i := 0; i < cap(pool.reqQueue); i++ {
					select {
					case pool.reqQueue <- &reqSlot{
						id:        fmt.Sprintf("refresh-check-%d", i),
						createdAt: time.Now(),
					}:
						filled++
					default:
						// If we can't fill the queue, there are active requests
						activeRequests := cap(pool.reqQueue) - filled
						log.Infof("🔴 Found %d active requests", activeRequests)

						// Return all the tokens we just acquired
						for j := 0; j < filled; j++ {
							<-pool.reqQueue
						}

						// Wait for active requests to complete
						time.Sleep(1 * time.Second)
						return fmt.Errorf("requests still in flight")
					}
				}

				// If we got here, we successfully filled the queue
				log.Info("✅ All request slots available - proceeding with refresh")

				// Return all tokens before proceeding
				for i := 0; i < filled; i++ {
					<-pool.reqQueue
				}
				return nil
			}, b)

			if err != nil {
				log.Warnf("⚠️ Proceeding with refresh despite active requests: %v", err)
				// Give a small grace period for any remaining requests
				time.Sleep(2 * time.Second)
			}

			log.Info("🔌 Refreshing connection to sequencer")
			if err := EstablishSequencerConnection(); err != nil {
				log.Errorf("❌ Failed to refresh connection: %v", err)
				connectionRefreshing.Store(false)
				continue
			}
			log.Info("✅ New connection established successfully")

			log.Info("🏊 Rebuilding stream pool")
			if err := RebuildStreamPool(); err != nil {
				log.Errorf("❌ Failed to rebuild stream pool: %v", err)
			}

			connectionRefreshing.Store(false)
			log.Info("✅ Connection refresh cycle completed successfully")
		}
	}
}
