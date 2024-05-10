package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	libp2ptls "github.com/libp2p/go-libp2p/p2p/security/tls"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"net/http"
	"os"
	"proto-snapshot-server/config"
	"time"
)

var rpctorelay host.Host
var SequencerId peer.ID
var routingDiscovery *routing.RoutingDiscovery

var activeConnections int

func handleConnectionEstablished(network network.Network, conn network.Conn) {
	activeConnections++
}

func handleConnectionClosed(network network.Network, conn network.Conn) {
	activeConnections--
}

func ConfigureRelayer() {
	var err error
	tcpAddr, _ := ma.NewMultiaddr("/ip4/0.0.0.0/tcp/9000")

	connManager, _ := connmgr.NewConnManager(
		100,
		400,
		connmgr.WithGracePeriod(5*time.Minute))

	scalingLimits := rcmgr.DefaultLimits

	libp2p.SetDefaultServiceLimits(&scalingLimits)

	scaledDefaultLimits := scalingLimits.AutoScale()

	cfg := rcmgr.PartialLimitConfig{
		System: rcmgr.ResourceLimits{
			Streams:         rcmgr.Unlimited,
			StreamsOutbound: rcmgr.Unlimited,
			StreamsInbound:  rcmgr.Unlimited,
			Conns:           rcmgr.Unlimited,
			ConnsInbound:    rcmgr.Unlimited,
			ConnsOutbound:   rcmgr.Unlimited,
			FD:              rcmgr.Unlimited,
			Memory:          rcmgr.LimitVal64(rcmgr.Unlimited),
		},
	}

	limits := cfg.Build(scaledDefaultLimits)

	limiter := rcmgr.NewFixedLimiter(limits)

	rm, err := rcmgr.NewResourceManager(limiter, rcmgr.WithMetricsDisabled())

	var pk crypto.PrivKey
	if config.SettingsObj.RelayerPrivateKey == "" {
		pk, err = generateAndSaveKey()
		if err != nil {
			log.Errorln("Unable to generate private key:  ", err)
		} else {
			log.Debugln("Private key not found, new private key generated")
		}
	} else {
		pkBytes, err := base64.StdEncoding.DecodeString(config.SettingsObj.RelayerPrivateKey)
		if err != nil {
			log.Errorln("Unable to decode private key: ", err)
		} else {
			pk, err = crypto.UnmarshalPrivateKey(pkBytes)
			if err != nil {
				log.Errorln("Unable to unmarshal private key: ", err)
			} else {
				log.Debugln("Private key unmarshalled")
			}
		}
	}

	rpctorelay, err = libp2p.New(
		libp2p.EnableRelay(),
		libp2p.Identity(pk),
		libp2p.ConnectionManager(connManager),
		libp2p.ListenAddrs(tcpAddr),
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
		return
	}

	log.Debugln("DashboardsEnabled: ", config.SettingsObj.DashboardEnabled)

	if config.SettingsObj.DashboardEnabled == "true" {
		log.Debugln("Starting Prometheus metrics server")
		go func() {
			http.Handle("/debug/metrics/prometheus", promhttp.Handler())
			log.Fatal(http.ListenAndServe(":5002", nil))
		}()
	}

	log.Debugln("id: ", rpctorelay.ID().String())
	rpctorelay.Network().Notify(&network.NotifyBundle{
		ConnectedF:    handleConnectionEstablished,
		DisconnectedF: handleConnectionClosed,
	})

	// Set up a Kademlia DHT for the service host

	kademliaDHT := ConfigureDHT(context.Background(), rpctorelay)

	routingDiscovery = routing.NewRoutingDiscovery(kademliaDHT)

	util.Advertise(context.Background(), routingDiscovery, config.SettingsObj.ClientRendezvousPoint)

	peerId := ConnectToPeer(context.Background(), routingDiscovery, config.SettingsObj.RelayerRendezvousPoint, rpctorelay, nil, 2*time.Minute)

	ConnectToSequencer(peerId)
}

func ConnectToSequencer(peerId peer.ID) {
	if peerId == "" {
		log.Debugln("Not connected to a relayer, establishing connection")
	}
	sequencerAddr, err := ma.NewMultiaddr(fmt.Sprintf("/p2p/%s/p2p-circuit/p2p/%s", peerId, config.SettingsObj.SequencerId))
	if err != nil {
		log.Debugln(err.Error())
		return
	}

	sequencerInfo, err := peer.AddrInfoFromP2pAddr(sequencerAddr)

	if err != nil {
		log.Errorln("Error converting MultiAddr to AddrInfo: ", err.Error())
	}

	SequencerId = sequencerInfo.ID

	if err := rpctorelay.Connect(context.Background(), *sequencerInfo); err != nil {
		log.Debugln("Failed to connect to the Sequencer:", err)
	} else {
		log.Debugln("Successfully connected to the Sequencer: ", sequencerAddr.String())
	}
}

func generateAndSaveKey() (crypto.PrivKey, error) {
	priv, _, err := crypto.GenerateKeyPair(crypto.RSA, 2048)
	if err != nil {
		return nil, err
	}
	encodedKey := PrivateKeyToString(priv)
	err = os.WriteFile("/config/key.txt", []byte(encodedKey), 0644)
	if err != nil {
		log.Fatal(err)
	}
	return priv, nil
}

func PrivateKeyToString(privKey crypto.PrivKey) string {
	privBytes, err := crypto.MarshalPrivateKey(privKey)
	if err != nil {
		log.Errorln("Unable to convert private key to string")
	}

	encodedKey := base64.StdEncoding.EncodeToString(privBytes)
	return encodedKey
}
