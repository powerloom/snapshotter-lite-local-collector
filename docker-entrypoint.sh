#!/bin/sh

# P2P Key Wait Integration for Local Collector
# This script waits for P2P keys to be available before starting the collector

SHARED_KEYS_DIR="/keys"
P2P_PRIVATE_KEY_FILE="$SHARED_KEYS_DIR/p2p_private_key"
P2P_READY_FILE="$SHARED_KEYS_DIR/p2p_ready"
MAX_WAIT_TIME=60
WAIT_INTERVAL=2

echo "🚀 Local Collector Entry Point with P2P Key Wait"
echo "=================================================="

# Function to check if P2P key file exists and is readable
check_p2p_key() {
    if [ -f "$P2P_PRIVATE_KEY_FILE" ] && [ -r "$P2P_PRIVATE_KEY_FILE" ]; then
        local key_content=$(cat "$P2P_PRIVATE_KEY_FILE")
        if [ ${#key_content} -eq 128 ]; then
            echo "✅ P2P private key found and valid (128 hex characters)"
            return 0
        else
            echo "⚠️  P2P private key found but invalid length: ${#key_content} (expected 128)"
            return 1
        fi
    else
        echo "⏳ P2P private key not found at: $P2P_PRIVATE_KEY_FILE"
        return 1
    fi
}

# Function to check if signal file exists
check_signal_file() {
    if [ -f "$P2P_READY_FILE" ]; then
        echo "✅ P2P ready signal file found"
        return 0
    else
        echo "⏳ P2P ready signal file not found at: $P2P_READY_FILE"
        return 1
    fi
}

# Main waiting logic
wait_for_p2p_keys() {
    echo "🔍 Checking for P2P keys in shared volume..."

    local elapsed_time=0

    while [ $elapsed_time -lt $MAX_WAIT_TIME ]; do
        if check_p2p_key && check_signal_file; then
            echo "🎉 P2P keys are ready! Starting local collector..."

            # Set the private key as environment variable for the local collector
            export LOCAL_COLLECTOR_PRIVATE_KEY=$(cat "$P2P_PRIVATE_KEY_FILE")

            # Also set peer ID if available
            local peer_id_file="$SHARED_KEYS_DIR/p2p_peer_id"
            if [ -f "$peer_id_file" ]; then
                export LOCAL_COLLECTOR_PEER_ID=$(cat "$peer_id_file")
                echo "🆔 Peer ID loaded: ${LOCAL_COLLECTOR_PEER_ID:0:20}..."
            fi

            echo "🔑 P2P private key loaded into environment"
            return 0
        fi

        echo "⏳ Waiting for P2P keys... (${elapsed_time}/${MAX_WAIT_TIME} seconds elapsed)"
        sleep $WAIT_INTERVAL
        elapsed_time=$((elapsed_time + WAIT_INTERVAL))
    done

    # If we reach here, we timed out
    echo "❌ Timeout: P2P keys not available after $MAX_WAIT_TIME seconds"
    echo "🔍 Checking if shared volume is properly mounted..."

    if [ ! -d "$SHARED_KEYS_DIR" ]; then
        echo "❌ Shared volume directory not found: $SHARED_KEYS_DIR"
        echo "   Please check Docker volume configuration"
    else
        echo "📁 Shared volume directory exists: $SHARED_KEYS_DIR"
        echo "📋 Contents:"
        ls -la "$SHARED_KEYS_DIR" || echo "   (Cannot list directory contents)"
    fi

    echo "💡 Possible solutions:"
    echo "   1. Ensure snapshotter container has started and completed P2P key generation"
    echo "   2. Check that shared volume is properly mounted in both containers"
    echo "   3. Verify snapshotter completed slot ID validation successfully"
    echo "   4. Consider using pre-configured P2P key in environment setup"

    exit 1
}

handle_exit() {
    EXIT_CODE=$?
    echo "Local collector container exited with code $EXIT_CODE. Restarting..."
    exit 1
}

trap 'handle_exit' EXIT HUP INT QUIT ABRT TERM

# Wait for P2P keys before starting the collector
wait_for_p2p_keys

# Start the local collector service
echo "🚀 Starting local collector service..."
exec /usr/local/bin/snapshotter-local-collector "$@" 