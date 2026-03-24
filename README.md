# Snapshotter Local Collector

The Local Collector is a Go-based service that receives snapshot submissions from snapshotter nodes via gRPC and forwards them to both the legacy centralized sequencer (via libp2p stream pool) and the decentralized sequencer validator (DSV) network (via gossipsub).

## Overview

The Local Collector acts as an intermediary service that:
- Receives snapshot submissions from snapshotter nodes over gRPC
- Optionally forwards submissions to the legacy centralized sequencer via libp2p stream pool (can be disabled)
- Broadcasts submissions to the DSV network via gossipsub (P2P)
- Implements dual submission pattern: each submission goes to both destinations (when centralized sequencer is enabled)

## Architecture

```
┌─────────────────┐         ┌──────────────────┐         ┌─────────────────┐
│  Snapshotter    │────────▶│  Local Collector  │────────▶│ Centralized     │
│  Node (Python)  │  gRPC   │     (Go)          │  Stream │ Sequencer       │
└─────────────────┘         └────────┬─────────┘  Pool    └─────────────────┘
                                       │
                                       │ Gossipsub
                                       ▼
                              ┌─────────────────┐
                              │  DSV Network    │
                              │  (P2P)          │
                              └─────────────────┘
```

## Features

- **Dual Submission**: Submissions are sent to both legacy centralized sequencer and DSV network (when centralized sequencer is enabled)
- **Configurable Centralized Sequencer**: Can disable centralized sequencer submissions via `CENTRALIZED_SEQUENCER_ENABLED` config
- **Mesh Concurrency Controls**: Rate limiting and resource limits for mesh submissions to prevent overwhelming the network
- **Non-blocking Semaphore**: Prevents indefinite blocking when system is at capacity
- **Gossipsub Integration**: P2P broadcasting to decentralized sequencer network
- **DHT-based Peer Discovery**: Automatic peer discovery for gossipsub mesh
- **Connection Management**: Configurable connection pool limits
- **Heartbeat Messages**: Two types of automatic heartbeat messages to help form and maintain mesh connectivity
- **Health Check Endpoint**: HTTP health check endpoint for mesh readiness monitoring

## Peer Discovery Architecture

The local collector uses a **multi-mechanism discovery approach** to ensure reliable peer discovery and mesh formation. All mechanisms use DHT (Distributed Hash Table) for peer finding, but search for different rendezvous strings/topic names to provide redundancy.

### Three Discovery Mechanisms

1. **Topic-based Discovery (Discovery Topic)**
   - Searches DHT for peers advertising on: `/powerloom/{prefix}/snapshot-submissions/0`
   - Purpose: Find peers via the discovery topic
   - Frequency: Every 30 seconds
   - Implementation: Inline connection logic in `initializeTopics()`

2. **Topic-based Discovery (Submissions Topic)**
   - Searches DHT for peers advertising on: `/powerloom/{prefix}/snapshot-submissions/all`
   - Purpose: Find peers via the submissions topic
   - Frequency: Every 30 seconds
   - Implementation: Inline connection logic in `initializeTopics()`

3. **Rendezvous Point Discovery**
   - Searches DHT for peers advertising on: `{RENDEZVOUS_POINT}` (e.g., `powerloom-dsv-devnet-alpha`)
   - Purpose: Find DSV nodes via a dedicated rendezvous string
   - Frequency: Every 30 seconds
   - Implementation: `discoverDSVPeers()` function called from `startDSVRendezvousDiscovery()`

### Two-Level Topic Architecture

The gossipsub mesh uses a two-level topic structure:

- **Discovery Topic** (`/powerloom/{prefix}/snapshot-submissions/0`): Used for peer discovery and network joining
  - Lightweight presence messages (heartbeats)
  - Helps establish initial mesh connections
  - Prevents race conditions during network formation
  - Heartbeat format: `Submissions: nil` (recognized and skipped by DSV nodes)

- **Submissions Topic** (`/powerloom/{prefix}/snapshot-submissions/all`): Used for actual snapshot data transmission
  - Full submission payloads
  - Primary data channel for DSV network
  - All snapshot submissions are published here
  - Heartbeat format: `Submissions: [{EpochId: 0, SnapshotCid: ""}]` (recognized as heartbeat but helps mesh formation)

### Why Multiple Discovery Mechanisms?

The redundancy ensures:
- **Reliability**: If one mechanism fails, others continue working
- **Faster Mesh Formation**: Multiple paths increase chances of finding peers quickly
- **Network Resilience**: Different discovery paths help maintain connectivity during network churn

All three mechanisms run concurrently and independently, connecting to discovered peers up to a limit of 3 connections per discovery round.

## Development

### Prerequisites

- Go 1.24.5 or later
- Protocol Buffers compiler (`protoc`)
- Go protobuf plugins:
  - `protoc-gen-go`
  - `protoc-gen-go-grpc`

### Building

```bash
cd snapshotter-lite-local-collector
go build ./cmd
```

### Running

The local collector can be run directly or via Docker Compose. See the main repository's `docker-compose.yaml.template` for configuration.

### Configuration

Key environment variables:

- `LOCAL_COLLECTOR_PORT`: gRPC server port (default: 50051)
- `LOCAL_COLLECTOR_P2P_PORT`: P2P port for libp2p (default: 9100)
- `LOCAL_COLLECTOR_PRIVATE_KEY`: Private key for libp2p peer identity
- `RENDEZVOUS_POINT`: Gossipsub rendezvous point for peer discovery
- `GOSSIPSUB_SNAPSHOT_SUBMISSION_PREFIX`: Topic prefix for gossipsub submissions
- `BOOTSTRAP_NODE_ADDRS`: Comma-separated list of bootstrap node addresses
- `PUBLIC_IP`: Public IP address for P2P connections
- `CONN_MANAGER_LOW_WATER`: Connection manager low water mark
- `CONN_MANAGER_HIGH_WATER`: Connection manager high water mark
- `WRITE_SEMAPHORE_TIMEOUT_SEC`: Timeout for semaphore acquisition (default: 5)
- `DOCKER_BRIDGE_GATEWAY_IPS`: Comma-separated Docker bridge gateway IPs to whitelist for inbound connections (required in Docker bridge mode — see below)

### Centralized Sequencer Configuration

- `CENTRALIZED_SEQUENCER_ENABLED`: Enable/disable submissions to centralized sequencer (default: `true`)
  - When set to `false`, all submissions to the centralized sequencer are disabled
  - Stream pool initialization is skipped when disabled
  - Mesh submissions continue to work normally
  - Useful when transitioning to mesh-only submissions or during centralized sequencer maintenance

### Mesh Submission Concurrency Controls

The following configuration options control concurrency and rate limiting for mesh submissions:

- `MESH_SUBMISSION_RATE_LIMIT`: Maximum submissions per second (default: 100)
  - Uses token bucket algorithm for rate limiting
  - Prevents overwhelming the gossipsub network with too many messages
  - Submissions exceeding the rate limit are queued (if queue space available)

- `MESH_SUBMISSION_BURST_SIZE`: Burst allowance for rate limiter (default: 200)
  - Allows short bursts above the rate limit
  - Helps handle traffic spikes without dropping submissions

- `MAX_MESH_PUBLISH_GOROUTINES`: Maximum concurrent mesh publish operations (default: 500)
  - Limits the number of goroutines actively publishing to the mesh
  - Prevents resource exhaustion from too many concurrent operations
  - Uses semaphore pattern for goroutine control

- `MESH_PUBLISH_QUEUE_SIZE`: Maximum queued mesh submissions (default: 1000)
  - Queue size for submissions waiting for rate limiter
  - When queue is full, submissions are dropped
  - Monitor `mesh_submissions_dropped` metric to detect queue saturation

### Monitoring Mesh Concurrency

The following metrics are tracked and logged periodically:

- `mesh_rate_limited`: Count of submissions rate limited
- `mesh_submissions_queued`: Current queue depth
- `mesh_publish_active`: Current active publish goroutines
- `mesh_submissions_dropped`: Submissions dropped due to full queue or timeout

These metrics are included in the periodic metrics report (every 15 seconds) and can be used to tune the concurrency control parameters.

### Regenerating Protobuf Files

When modifying the protobuf definition file (`pkgs/proto/submission.proto`), you need to regenerate the Go protobuf files.

**Prerequisites:**

1. **Install Protocol Buffers compiler:**
   ```bash
   # macOS
   brew install protobuf
   
   # Or download from https://github.com/protocolbuffers/protobuf/releases
   ```

2. **Install Go protobuf plugins:**
   ```bash
   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
   ```

3. **Ensure plugins are in your PATH:**
   ```bash
   # Add Go bin directory to PATH (if not already)
   export PATH="$PATH:$(go env GOPATH)/bin"
   ```

**Regeneration Steps:**

1. **Navigate to the proto directory:**
   ```bash
   cd snapshotter-lite-local-collector/pkgs/proto
   ```

2. **Regenerate Go protobuf files:**
   ```bash
   protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       submission.proto
   ```

3. **Move generated files to the correct location:**
   
   The protoc command generates files in `pkgs/proto/`, but the code expects them in `pkgs/`. Move the generated files:
   ```bash
   mv pkgs/proto/submission.pb.go pkgs/
   mv pkgs/proto/submission_grpc.pb.go pkgs/
   ```

   Or regenerate with output directory specified:
   ```bash
   cd snapshotter-lite-local-collector
   protoc --go_out=pkgs --go_opt=paths=source_relative \
       --go-grpc_out=pkgs --go-grpc_opt=paths=source_relative \
       pkgs/proto/submission.proto
   ```

**Note:** The protobuf files define the gRPC service interface between the snapshotter node (Python) and the local collector (Go). After modifying the proto file, ensure both sides regenerate their protobuf files to maintain compatibility.

## Testing

Run tests with:
```bash
go test ./...
```

## Project Structure

```
snapshotter-lite-local-collector/
├── cmd/                    # Main application entry point
├── pkgs/
│   ├── proto/             # Protobuf definition files
│   │   └── submission.proto
│   ├── service/           # Core service logic
│   │   ├── msg_server.go  # gRPC message server
│   │   ├── initialization.go
│   │   ├── discovery.go   # DHT peer discovery
│   │   └── ...
│   ├── submission.pb.go   # Generated protobuf code
│   └── submission_grpc.pb.go  # Generated gRPC code
├── config/                # Configuration management
└── README.md             # This file
```

## Health Check Endpoint

The local collector provides HTTP health check endpoints for monitoring mesh status and readiness:

### Endpoints

- **`GET /health`**: Returns detailed health status with mesh metrics
- **`GET /ready`**: Returns readiness status (200 OK if mesh is healthy, 503 if not)

### Health Check Response

```json
{
  "status": "healthy",
  "mesh_state": "healthy",
  "mesh_ready": true,
  "discovery_peers": 3,
  "submissions_peers": 3,
  "total_connected": 5,
  "uptime_seconds": 3600,
  "total_pruning_events": 0,
  "timestamp": "2025-12-03T16:30:00Z"
}
```

### Using Health Check for Snapshotter Node Dependency

The snapshotter node can wait for the local collector to be ready before starting:

```bash
# Wait for local collector to be ready
until curl -f http://localhost:8080/ready; do
  echo "Waiting for local collector mesh to form..."
  sleep 5
done

# Start snapshotter node
python -m snapshotter.main
```

Or in Docker Compose:

```yaml
snapshotter-node:
  depends_on:
    snapshotter-local-collector:
      condition: service_healthy
  healthcheck:
    test: ["CMD", "curl", "-f", "http://snapshotter-local-collector:8080/ready"]
```

### Heartbeat Messages

The local collector automatically publishes heartbeat messages every 10 seconds to help form and maintain the gossipsub mesh. These are independent of the snapshotter node (not via gRPC) and use two different formats optimized for each topic:

#### Type 1: Discovery Topic Heartbeat (`/powerloom/{prefix}/snapshot-submissions/0`)

- **Format**: `Submissions: nil` (null submissions array)
- **Purpose**: Lightweight presence announcement for peer discovery
- **DSV Recognition**: Skipped immediately at queue level (no processing overhead)
- **Detection**: DSV node checks `epoch_id == 0 && submissions == nil`

#### Type 2: Submissions Topic Heartbeat (`/powerloom/{prefix}/snapshot-submissions/all`)

- **Format**: `Submissions: [{EpochId: 0, SnapshotCid: ""}]` (non-nil array with empty CID)
- **Purpose**: Helps mesh formation on submissions topic while maintaining presence
- **DSV Recognition**: Queued but skipped during validation (recognized as heartbeat)
- **Detection**: DSV node checks `EpochId == 0 && SnapshotCid == ""` in dequeuer validation

Both heartbeat types:
- Are recognized by DSV nodes and skipped (not processed as real submissions)
- Help establish mesh connectivity during startup
- Maintain active presence in the mesh to prevent pruning
- Use project ID `test:mesh-formation:local-collector` for identification

This ensures the mesh forms quickly even before the snapshotter node starts sending real submissions, and prevents the mesh from being pruned due to inactivity.

## Integration with Snapshotter Node

The local collector receives submissions from snapshotter nodes via gRPC. The snapshotter node must:

1. Use the same protobuf definition (`submission.proto`)
2. Include `protocolState` and `nodeVersion` fields in submissions
3. Connect to the local collector's gRPC endpoint
4. **Recommended**: Wait for `/ready` endpoint before starting to ensure mesh is formed

See the main repository's README for protobuf regeneration instructions for the Python snapshotter node.

## Mesh Lifecycle Monitoring

The local collector includes comprehensive monitoring for gossipsub mesh health, including lifecycle hooks and detailed logging for when the mesh is pruned or recovers.

### Mesh States

The mesh can be in one of three states:

- **`healthy`**: Both discovery and submissions topics have 2+ peers
- **`degraded`**: One or both topics have fewer than 2 peers
- **`pruned`**: One or both topics have 0 peers (critical - messages won't propagate)

### Key Log Messages to Monitor

#### Critical Alerts (ERROR level)

1. **Mesh Pruning Event**:
   ```
   🚨 MESH STATE TRANSITION: healthy -> pruned
   ```
   - Indicates the local collector has been pruned from the gossipsub mesh
   - Messages published after this will not reach DSV nodes
   - Includes: peer counts, uptime, total pruning events

2. **Zero-Peer Publish Attempt**:
   ```
   🚨 CRITICAL: Publishing to gossipsub with 0 peers in topic mesh - messages will not propagate!
   ```
   - Triggered when attempting to publish with no peers in mesh
   - Includes: mesh state, pruning history, uptime

3. **Mesh Pruning Detection**:
   ```
   🚨 CRITICAL: Mesh pruned - no peers in gossipsub mesh!
   ```
   - Periodic check detects pruning
   - Includes: consecutive low peer counts, total pruning events

#### Warning Messages (WARN level)

1. **Low Peer Count**:
   ```
   ⚠️ Publishing with low peer count - mesh may be degrading
   ```
   - Published with 1 peer (degraded state)

2. **Mesh Recovery Attempt**:
   ```
   🔄 Attempting mesh recovery...
   ```
   - Automatic recovery triggered when pruning detected

#### Informational Messages (INFO level)

1. **Mesh Recovery**:
   ```
   ✅ Mesh recovered - peer connections restored
   ```
   - Mesh has recovered from pruned/degraded state

2. **Mesh Lifecycle Events**:
   ```
   🔔 Mesh lifecycle event
   ```
   - All mesh state transitions and peer count changes
   - Includes: event type, state, peer counts, metrics

3. **Mesh Health Status** (every 5 minutes):
   ```
   📊 Mesh health status
   ```
   - Periodic health check with full metrics

### Log Filtering Commands

#### Monitor Mesh State Transitions
```bash
# Docker logs
docker logs -f <container-name> 2>&1 | grep -E "MESH STATE TRANSITION|mesh_state_transition"

# Or with log aggregation
grep -E "MESH STATE TRANSITION|mesh_state_transition" /path/to/logs/*.log
```

#### Track Pruning Events
```bash
# All pruning-related logs
docker logs -f <container-name> 2>&1 | grep -E "pruned|pruning|CRITICAL.*mesh"

# Count pruning events
docker logs <container-name> 2>&1 | grep -c "total_pruning_events"
```

#### Monitor Mesh Health
```bash
# Mesh health status (every 5 minutes)
docker logs -f <container-name> 2>&1 | grep "📊 Mesh health status"

# All lifecycle events
docker logs -f <container-name> 2>&1 | grep "🔔 Mesh lifecycle event"
```

#### Track Zero-Peer Publish Attempts
```bash
# Critical zero-peer publishes
docker logs -f <container-name> 2>&1 | grep "CRITICAL.*0 peers in topic mesh"
```

### Slack Alerts (Recommended)

The local collector supports Slack webhook alerts for critical mesh events. This is the fastest way to get notified when the mesh is pruned.

#### Setup

1. **Create a Slack Incoming Webhook**:
   - Go to https://api.slack.com/apps
   - Create a new app or select existing
   - Enable "Incoming Webhooks"
   - Click "Add New Webhook to Workspace"
   - **Select the channel** where you want alerts (this is the only configuration needed)
   - Copy the webhook URL
   - **Note**: You don't need to configure any JSON structure in Slack's editor - the local collector sends formatted JSON automatically

2. **Configure Environment Variable**:
   ```bash
   export SLACK_WEBHOOK_URL=https://hooks.slack.com/services/YOUR/WEBHOOK/URL
   ```

3. **Add to Docker Compose**:
   ```yaml
   environment:
     - SLACK_WEBHOOK_URL=${SLACK_WEBHOOK_URL}
   ```

#### Alert Triggers

Slack alerts are sent for:
- **Mesh pruned** (`mesh_state_transition:*->pruned`)
- **Zero-peer publish attempts** (`zero_peer_publish_attempt`)
- **Extended degraded state** (degraded for 10+ consecutive checks)

#### Alert Format

Slack alerts include:
- Current mesh state
- Peer counts (discovery, submissions, total connected)
- Consecutive low peer count periods
- Total pruning events since startup
- Last pruning time
- Uptime
- Event type

Example alert:
```
🚨 Gossipsub Mesh Alert: mesh_state_transition:healthy->pruned
State: pruned
Severity: CRITICAL
Discovery Peers: 0
Submissions Peers: 0
Total Connected: 0
Consecutive Low: 784
Total Pruning Events: 1
Last Pruning: 2025-12-03T09:08:23Z
Uptime: 7h 32m
```

### Setting Up Other Alerts

#### Example: Prometheus Alert Rules

```yaml
groups:
  - name: gossipsub_mesh
    rules:
      - alert: MeshPruned
        expr: increase(mesh_pruning_events_total[5m]) > 0
        annotations:
          summary: "Local collector pruned from gossipsub mesh"
          description: "Mesh state transitioned to pruned. Messages will not propagate."
      
      - alert: MeshDegraded
        expr: mesh_peer_count < 2
        for: 2m
        annotations:
          summary: "Gossipsub mesh degraded"
          description: "Peer count below threshold: {{ $value }}"
```

#### Example: Log-Based Alerting (ELK/Logstash)

```ruby
# Logstash filter for mesh pruning
if [message] =~ /MESH STATE TRANSITION.*pruned/ {
  mutate {
    add_tag => [ "alert", "mesh_pruned" ]
  }
}
```

#### Example: Custom Hook for External Alerting

```go
// Register custom hook in your monitoring code
server.RegisterMeshLifecycleHook(func(event string, metrics MeshHealthMetrics) {
    if metrics.State == MeshStatePruned {
        // Send to your alerting system
        sendAlert("Mesh Pruned", map[string]interface{}{
            "uptime_seconds": metrics.Uptime.Seconds(),
            "pruning_events": metrics.TotalPruningEvents,
            "peer_counts": map[string]int{
                "discovery": metrics.DiscoveryPeerCount,
                "submissions": metrics.SubmissionsPeerCount,
            },
        })
    }
})
```

### Log Fields Reference

All mesh-related logs include these fields:

- `state`: Current mesh state (`healthy`, `degraded`, `pruned`)
- `discovery_peers`: Number of peers in discovery topic mesh
- `submission_peers`: Number of peers in submissions topic mesh
- `total_connected`: Total libp2p connections (may include non-mesh peers)
- `consecutive_low`: Number of consecutive checks with low/zero peers
- `total_pruning_events`: Total number of times mesh was pruned since startup
- `uptime_seconds`: Time since local collector started
- `host_id`: Local collector's libp2p peer ID

### Expected Behavior

- **Normal Operation**: Mesh state should be `healthy` with 2+ peers in both topics
- **Temporary Degradation**: Brief periods of `degraded` state during network churn are normal
- **Pruning**: If mesh becomes `pruned`, automatic recovery is attempted every 30 seconds
- **Recovery**: Mesh should recover within 1-2 minutes if DSV nodes are available

### Troubleshooting Mesh Issues

1. **Mesh stays pruned**:
   - Check if DSV nodes are online and advertising on rendezvous point
   - Verify `RENDEZVOUS_POINT` matches DSV node configuration
   - Check DHT bootstrap status
   - Review connection manager limits (may be pruning DSV peers)

2. **Frequent pruning**:
   - Check `total_pruning_events` - increasing count indicates recurring issues
   - Review `uptime_seconds` - pruning after 1-2 days suggests gossipsub scoring issues
   - Verify heartbeat publishing is working (should publish every 10 seconds)

3. **Low peer count but not zero**:
   - `degraded` state - mesh is partially connected
   - May indicate network issues or DSV node availability problems
   - Monitor for transition to `pruned` state

## Troubleshooting

### Protobuf Generation Issues

- **Error: `protoc-gen-go: program not found`**: Ensure `protoc-gen-go` is installed and in your PATH
- **Error: `protoc-gen-go-grpc: program not found`**: Ensure `protoc-gen-go-grpc` is installed and in your PATH
- **Generated files in wrong location**: Move them manually or adjust the protoc command output directory

### Connection Issues

- **Cannot connect to bootstrap nodes**: Verify `BOOTSTRAP_NODE_ADDRS` are correct and nodes are reachable
- **No peers in gossipsub mesh**: Check DHT bootstrap status and rendezvous point configuration
- **Stream pool connection failures**: Verify centralized sequencer endpoint configuration

### Docker Bridge NAT and RFC1918 Connection Gater

When running in Docker bridge networking mode (the default), Docker NAT rewrites the source IP of **all** inbound TCP connections to the bridge gateway IP (e.g. `172.21.0.1`). The RFC1918 connection gater sees this private IP and rejects the connection at `InterceptAccept`, before the libp2p security handshake starts. The remote peer sees `failed to negotiate security protocol: EOF`.

**Symptoms:**
- Logs show: `connection gater: reject inbound (InterceptAccept) remote=/ip4/172.x.x.1/tcp/...`
- Remote peers see: `failed to negotiate security protocol: EOF` or `dial backoff`

**Fix:** Set `DOCKER_BRIDGE_GATEWAY_IPS` to the gateway IP of the Docker network the container is attached to:
```bash
# Find the gateway IP
docker network inspect <network-name> | grep Gateway

# Add to .env
DOCKER_BRIDGE_GATEWAY_IPS=172.21.0.1
```

This whitelists the gateway IP in the connection gater for inbound connections only. Outbound blocking of private IPs (required by Hetzner) is unaffected.

### Mesh Pruning Issues

See the [Mesh Lifecycle Monitoring](#mesh-lifecycle-monitoring) section above for detailed troubleshooting.

## License

See the main repository LICENSE file.

