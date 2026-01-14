package service

import (
	"context"
	"encoding/hex"
	"fmt"
	"proto-snapshot-server/config"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	circuitv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
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
	P2PHost              host.Host
	SequencerID          peer.ID
	sequencerMu          sync.RWMutex
	ConnManager          *connmgr.BasicConnMgr
	TcpAddr              ma.Multiaddr
	rm                   network.ResourceManager
	connectionRefreshing atomic.Bool
	lastAllConnLostLog   time.Time // Throttle "all connections lost" error logs
	allConnLostLogMu     sync.Mutex
)

// Thread-safe getter for connection state
func GetSequencerConnection() (host.Host, peer.ID, error) {
	sequencerMu.RLock()
	defer sequencerMu.RUnlock()

	if P2PHost == nil || SequencerID.String() == "" {
		return nil, "", fmt.Errorf("sequencer connection not established")
	}

	return P2PHost, SequencerID, nil
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

	// Use configurable connection manager limits (defaults match DSV nodes: 1000/4000)
	// CRITICAL: Use very permissive limits to prevent aggressive pruning
	// LowWater=1 means we'll never prune when we have at least 1 connection
	// HighWater=1000 means we'll only prune if we somehow get 1000+ connections
	// This prevents the connection manager from closing connections when we have few peers
	// The exact 1-hour pruning issue suggests connection manager is being too aggressive
	connLowWater := config.SettingsObj.ConnManagerLowWater
	connHighWater := config.SettingsObj.ConnManagerHighWater
	if connLowWater == 0 {
		connLowWater = 1 // Very low default - never prune when we have at least 1 connection
	}
	if connHighWater == 0 {
		connHighWater = 1000 // High default - only prune if we get 1000+ connections
	}

	ConnManager, _ = connmgr.NewConnManager(
		connLowWater,
		connHighWater,
		connmgr.WithGracePeriod(1*time.Minute))
	log.Infof("Connection manager configured: LowWater=%d, HighWater=%d (permissive mode to prevent 1-hour pruning)", connLowWater, connHighWater)

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
			ConnsInbound:    rcmgr.Unlimited, // Allow many inbound connections
			FD:              rcmgr.Unlimited,
			Memory:          rcmgr.LimitVal64(rcmgr.Unlimited),
		},
		Transient: rcmgr.ResourceLimits{
			StreamsOutbound: rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
			Streams:         rcmgr.Unlimited,
			Conns:           rcmgr.Unlimited,
			ConnsOutbound:   rcmgr.Unlimited,
			ConnsInbound:    rcmgr.Unlimited, // Allow transient inbound connections
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

	// Create RFC1918 connection gater to block private IP connections
	// This is required by Hetzner to prevent scanning of internal networks
	rfc1918Gater := &RFC1918ConnectionGater{}

	opts := []libp2p.Option{
		libp2p.EnableRelay(),
		libp2p.ConnectionManager(ConnManager),
		libp2p.ConnectionGater(rfc1918Gater), // Block RFC1918 connections at dial level
		libp2p.ListenAddrs(TcpAddr),
		libp2p.ResourceManager(rm),
		libp2p.Security(libp2ptls.ID, libp2ptls.New),
		libp2p.Security(noise.ID, noise.New),
		libp2p.DefaultTransports,
		libp2p.NATPortMap(),
		libp2p.EnableRelayService(),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.Muxer(yamux.ID, yamux.DefaultTransport),
	}

	// Use private key if provided to maintain consistent peer ID across restarts
	if config.SettingsObj.LocalCollectorPrivateKey != "" {
		// Parse hex-encoded Ed25519 private key (128 hex chars = 64 bytes)
		keyBytes, err := hex.DecodeString(config.SettingsObj.LocalCollectorPrivateKey)
		if err != nil {
			log.Warnf("Failed to decode private key from hex: %v, generating new identity", err)
		} else {
			privKey, err := crypto.UnmarshalEd25519PrivateKey(keyBytes)
			if err != nil {
				log.Warnf("Failed to unmarshal Ed25519 private key: %v, generating new identity", err)
			} else {
				opts = append(opts, libp2p.Identity(privKey))
				log.Info("Using LOCAL_COLLECTOR_PRIVATE_KEY for libp2p identity")
			}
		}
	} else {
		log.Info("No LOCAL_COLLECTOR_PRIVATE_KEY configured, libp2p will generate a new identity")
	}

	// Parse bootstrap nodes for AutoRelay (when PUBLIC_IP is not set)
	var staticRelays []peer.AddrInfo
	if config.SettingsObj.PublicIP == "" && len(config.SettingsObj.BootstrapNodeAddrs) > 0 {
		for _, bootstrapAddr := range config.SettingsObj.BootstrapNodeAddrs {
			if bootstrapAddr == "" {
				continue
			}
			peerMA, err := ma.NewMultiaddr(bootstrapAddr)
			if err != nil {
				log.Debugf("Failed to parse bootstrap addr for AutoRelay: %v", err)
				continue
			}
			// Skip RFC1918 addresses
			if HasRFC1918Address(peerMA) {
				continue
			}
			peerInfo, err := peer.AddrInfoFromP2pAddr(peerMA)
			if err != nil {
				log.Debugf("Failed to parse bootstrap peer info for AutoRelay: %v", err)
				continue
			}
			// Filter RFC1918 addresses from peer info
			if len(peerInfo.Addrs) > 0 {
				filteredAddrs, _ := FilterRFC1918Multiaddrs(peerInfo.Addrs)
				if len(filteredAddrs) == 0 {
					continue
				}
				peerInfo.Addrs = filteredAddrs
			}
			staticRelays = append(staticRelays, *peerInfo)
		}
		if len(staticRelays) > 0 {
			opts = append(opts, libp2p.EnableAutoRelayWithStaticRelays(staticRelays))
			log.Infof("AutoRelay enabled with %d bootstrap nodes as static relays (PUBLIC_IP not set)", len(staticRelays))
		}
	}

	// Add public IP address if configured (like DSV nodes do)
	// This ensures we advertise the correct public IP and port in DHT
	if config.SettingsObj.PublicIP != "" {
		publicAddr, err := ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%s", config.SettingsObj.PublicIP, config.SettingsObj.LocalCollectorP2PPort))
		if err != nil {
			log.Errorf("Failed to create public multiaddr: %v", err)
		} else {
			opts = append(opts, libp2p.AddrsFactory(func(addrs []ma.Multiaddr) []ma.Multiaddr {
				// Add the public address to the list - this is what gets advertised in DHT
				return append(addrs, publicAddr)
			}))
			log.Debugf("Advertising public IP %s on port %s in DHT", config.SettingsObj.PublicIP, config.SettingsObj.LocalCollectorP2PPort)
		}
	}

	P2PHost, err = libp2p.New(opts...)

	if err != nil {
		log.Debugln("Error instantiating libp2p host: ", err.Error())
		return err
	}

	P2PHost.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, conn network.Conn) {
			totalConnections := len(P2PHost.Network().Peers())
			log.Debugf("🔌 P2P peer connected: %s, Addr: %s, Total connections: %d",
				conn.RemotePeer(), conn.RemoteMultiaddr(), totalConnections)
			// Tag all incoming connections to protect them from pruning
			// This ensures peers that connect to us (not just ones we discover) are protected
			if ConnManager != nil {
				ConnManager.TagPeer(conn.RemotePeer(), "inbound-peer", 25) // Low priority but still protected
				log.Debugf("Tagged peer %s with 'inbound-peer' tag", conn.RemotePeer())
			}
		},
		DisconnectedF: func(_ network.Network, conn network.Conn) {
			totalConnections := len(P2PHost.Network().Peers())

			// CRITICAL: Immediately invalidate all streams on this connection
			// This prevents dead streams from being used after connection closes
			// Only do this if centralized sequencer is enabled
			if config.SettingsObj.CentralizedSequencerEnabled {
				pool := GetLibp2pStreamPool()
				if pool != nil && conn.RemotePeer() == SequencerID {
					pool.InvalidateStreamsForConnection(conn)
				}
			}

			// NOTE: Direction tells us who INITIATED the connection, not who closed it
			// DirOutbound = we dialed them (we initiated connection)
			// DirInbound = they dialed us (peer initiated connection)
			// We cannot determine who closed the connection from Direction alone
			connectionDirection := "unknown"
			weInitiatedConnection := false
			if conn.Stat().Direction == network.DirOutbound {
				connectionDirection = "outbound (we dialed them)"
				weInitiatedConnection = true
			} else if conn.Stat().Direction == network.DirInbound {
				connectionDirection = "inbound (they dialed us)"
				weInitiatedConnection = false
			}

			log.Debugf("🔌 P2P peer disconnected: %s, Addr: %s, Connection was: %s, Remaining connections: %d",
				conn.RemotePeer(), conn.RemoteMultiaddr(), connectionDirection, totalConnections)

			// Log if this is a critical disconnection (mesh peer or last connection)
			// Throttle logging to prevent spam when multiple connections disconnect simultaneously
			if totalConnections == 0 {
				allConnLostLogMu.Lock()
				shouldLog := time.Since(lastAllConnLostLog) > 1*time.Minute
				if shouldLog {
					lastAllConnLostLog = time.Now()
					allConnLostLogMu.Unlock()
					log.Error("🚨 CRITICAL: All connections lost! Cannot determine who closed them from Direction alone - could be connection manager, network issue, or peer-initiated")
				} else {
					allConnLostLogMu.Unlock()
					log.Debugf("All connections lost (throttled log - last logged %v ago)", time.Since(lastAllConnLostLog))
				}
			}

			// Track disconnection for diagnostics (used in Slack alerts)
			// Note: weInitiatedConnection means we initiated the CONNECTION, not the disconnect
			if deps.serverInstance != nil {
				deps.serverInstance.recordDisconnection(weInitiatedConnection)
			}
		},
	})

	return nil
}

// EstablishSequencerConnection should only be called during initialization
// or explicit reconnection logic, not during stream operations
func EstablishSequencerConnection() error {
	sequencerMu.Lock()
	defer sequencerMu.Unlock()

	// CRITICAL: Only create host if it doesn't exist
	// DO NOT close existing host - it's shared with gossipsub!
	// Closing the host would kill all gossipsub connections
	if P2PHost == nil {
		// 1. Create properly configured host (only if it doesn't exist)
		if err := CreateLibP2pHost(); err != nil {
			return fmt.Errorf("failed to create libp2p host: %w", err)
		}
		// Update deps.hostConn if gossipsub hasn't been initialized yet
		if deps.hostConn == nil {
			deps.hostConn = P2PHost
		}
	}

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

	// 4. Check if we're already connected to the right sequencer
	if SequencerID == sequencerInfo.ID {
		// Check connection status
		if P2PHost.Network().Connectedness(SequencerID) == network.Connected {
			log.Debugf("Already connected to sequencer %s, skipping refresh", SequencerID)
			return nil
		}
	}

	// 5. Close ONLY the sequencer connection (not the entire host!)
	if SequencerID != "" && P2PHost != nil {
		if err := P2PHost.Network().ClosePeer(SequencerID); err != nil {
			log.Debugf("Error closing connection to previous sequencer: %v", err)
		}
	}

	// 6. Set sequencer ID
	SequencerID = sequencerInfo.ID
	if SequencerID.String() == "" {
		return fmt.Errorf("empty sequencer ID")
	}

	// 7. Establish connection with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := P2PHost.Connect(ctx, *sequencerInfo); err != nil {
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
			// Skip connection refresh if centralized sequencer is disabled
			if !config.SettingsObj.CentralizedSequencerEnabled {
				log.Debug("Skipping connection refresh - centralized sequencer disabled")
				continue
			}

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

			// Small delay to ensure connection is fully established before rebuilding pool
			time.Sleep(500 * time.Millisecond)

			log.Info("🏊 Rebuilding stream pool")
			if err := RebuildStreamPool(); err != nil {
				log.Errorf("❌ Failed to rebuild stream pool: %v", err)
				// Don't continue - connection refresh failed, streams will be created on-demand
				connectionRefreshing.Store(false)
				continue
			}

			connectionRefreshing.Store(false)
			log.Info("✅ Connection refresh cycle completed successfully")
		}
	}
}
