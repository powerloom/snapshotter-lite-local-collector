# Snapshotter Local Collector

The Local Collector is a Go-based service that receives snapshot submissions from snapshotter nodes via gRPC and forwards them to both the legacy centralized sequencer (via libp2p stream pool) and the decentralized sequencer validator (DSV) network (via gossipsub).

## Overview

The Local Collector acts as an intermediary service that:
- Receives snapshot submissions from snapshotter nodes over gRPC
- Forwards submissions to the legacy centralized sequencer via libp2p stream pool
- Broadcasts submissions to the DSV network via gossipsub (P2P)
- Implements dual submission pattern: each submission goes to both destinations

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

- **Dual Submission**: Submissions are sent to both legacy centralized sequencer and DSV network
- **Non-blocking Semaphore**: Prevents indefinite blocking when system is at capacity
- **Gossipsub Integration**: P2P broadcasting to decentralized sequencer network
- **DHT-based Peer Discovery**: Automatic peer discovery for gossipsub mesh
- **Connection Management**: Configurable connection pool limits

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

## Integration with Snapshotter Node

The local collector receives submissions from snapshotter nodes via gRPC. The snapshotter node must:

1. Use the same protobuf definition (`submission.proto`)
2. Include `protocolState` and `nodeVersion` fields in submissions
3. Connect to the local collector's gRPC endpoint

See the main repository's README for protobuf regeneration instructions for the Python snapshotter node.

## Troubleshooting

### Protobuf Generation Issues

- **Error: `protoc-gen-go: program not found`**: Ensure `protoc-gen-go` is installed and in your PATH
- **Error: `protoc-gen-go-grpc: program not found`**: Ensure `protoc-gen-go-grpc` is installed and in your PATH
- **Generated files in wrong location**: Move them manually or adjust the protoc command output directory

### Connection Issues

- **Cannot connect to bootstrap nodes**: Verify `BOOTSTRAP_NODE_ADDRS` are correct and nodes are reachable
- **No peers in gossipsub mesh**: Check DHT bootstrap status and rendezvous point configuration
- **Stream pool connection failures**: Verify centralized sequencer endpoint configuration

## License

See the main repository LICENSE file.

