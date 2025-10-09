package config

import (
	"os"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
)

var SettingsObj *Settings

type Settings struct {
	SequencerID            string
	RelayerRendezvousPoint string
	ClientRendezvousPoint  string
	RelayerPrivateKey      string
	PowerloomReportingUrl  string
	SignerAccountAddress   string
	PortNumber             string
	TrustedRelayersListUrl string
	DataMarketAddress      string
	MaxStreamPoolSize      int
	DataMarketInRequest    bool

	// Gossipsub Configuration
	GossipsubSnapshotSubmissionPrefix string

	// Stream Pool Configuration
	StreamHealthCheckTimeout time.Duration
	StreamWriteTimeout       time.Duration
	MaxWriteRetries          int
	MaxConcurrentWrites      int
	MaxStreamQueueSize       int

	// Connection management settings
	ConnectionRefreshInterval time.Duration
	BootstrapNodeAddr         string
	LocalCollectorP2PPort     string
	RendezvousPoint           string
	ConnManagerLowWater       int
	ConnManagerHighWater      int
	PublicIP                  string
}

func LoadConfig() {
	config := Settings{}

	// Required fields
	if port := os.Getenv("LOCAL_COLLECTOR_PORT"); port != "" {
		config.PortNumber = port
	} else {
		config.PortNumber = "50051" // Default value
	}
	config.RendezvousPoint = getEnvWithDefault("RENDEZVOUS_POINT", "powerloom-snapshot-sequencer-network")
	config.GossipsubSnapshotSubmissionPrefix = getEnvWithDefault("GOSSIPSUB_SNAPSHOT_SUBMISSION_PREFIX", "/powerloom/snapshot-submissions")

	if contract := os.Getenv("DATA_MARKET_CONTRACT"); contract == "" {
		log.Fatal("DATA_MARKET_CONTRACT environment variable is required")
	} else {
		config.DataMarketAddress = contract
	}
	if value := os.Getenv("DATA_MARKET_IN_REQUEST"); value == "true" {
		config.DataMarketInRequest = true
	} else {
		config.DataMarketInRequest = false
	}
	// Optional fields with defaults
	config.PowerloomReportingUrl = os.Getenv("POWERLOOM_REPORTING_URL")
	config.SignerAccountAddress = os.Getenv("SIGNER_ACCOUNT_ADDRESS")
	config.TrustedRelayersListUrl = getEnvWithDefault("TRUSTED_RELAYERS_LIST_URL", "https://raw.githubusercontent.com/PowerLoom/snapshotter-lite-local-collector/feat/trusted-relayers/relayers.json")

	// Load private key from file or env
	config.RelayerPrivateKey = loadPrivateKey()

	// Numeric values with defaults
	config.MaxStreamPoolSize = getEnvAsInt("MAX_STREAM_POOL_SIZE", 100)
	config.StreamHealthCheckTimeout = time.Duration(getEnvAsInt("STREAM_HEALTH_CHECK_TIMEOUT_MS", 5000)) * time.Millisecond
	config.StreamWriteTimeout = time.Duration(getEnvAsInt("STREAM_WRITE_TIMEOUT_MS", 5000)) * time.Millisecond
	config.MaxWriteRetries = getEnvAsInt("MAX_WRITE_RETRIES", 5)
	config.MaxConcurrentWrites = getEnvAsInt("MAX_CONCURRENT_WRITES", 100)
	config.MaxStreamQueueSize = getEnvAsInt("MAX_STREAM_QUEUE_SIZE", 1000)

	// Add connection refresh interval setting (default 5 minutes)
	config.ConnectionRefreshInterval = time.Duration(getEnvAsInt("CONNECTION_REFRESH_INTERVAL_SEC", 300)) * time.Second
	config.BootstrapNodeAddr = os.Getenv("BOOTSTRAP_NODE_ADDR")
	config.LocalCollectorP2PPort = getEnvWithDefault("LOCAL_COLLECTOR_P2P_PORT", "9100")

	config.ConnManagerLowWater = getEnvAsInt("CONN_MANAGER_LOW_WATER", 10000)
	config.ConnManagerHighWater = getEnvAsInt("CONN_MANAGER_HIGH_WATER", 40000)
	config.PublicIP = os.Getenv("PUBLIC_IP")

	SettingsObj = &config
}

func getEnvWithDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
		log.Warnf("Invalid value for %s, using default: %d", key, defaultValue)
	}
	return defaultValue
}

// GetSnapshotSubmissionTopics returns the discovery and submissions topic names
func (s *Settings) GetSnapshotSubmissionTopics() (discoveryTopic, submissionsTopic string) {
	discoveryTopic = s.GossipsubSnapshotSubmissionPrefix + "/0"
	submissionsTopic = s.GossipsubSnapshotSubmissionPrefix + "/all"
	return discoveryTopic, submissionsTopic
}

func loadPrivateKey() string {
	// Try loading from file first
	if keyBytes, err := os.ReadFile("/keys/key.txt"); err == nil {
		return string(keyBytes)
	}
	// Fall back to environment variable
	return os.Getenv("RELAYER_PRIVATE_KEY")
}
