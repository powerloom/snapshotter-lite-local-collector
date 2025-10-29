#!/bin/sh

# Local Collector Entry Point
# P2P keys are provided via environment variables

handle_exit() {
    EXIT_CODE=$?
    echo "Local collector container exited with code $EXIT_CODE. Restarting..."
    exit 1
}

trap 'handle_exit' EXIT HUP INT QUIT ABRT TERM

# Check if LOCAL_COLLECTOR_PRIVATE_KEY is available
if [ -n "$LOCAL_COLLECTOR_PRIVATE_KEY" ]; then
    echo "✅ LOCAL_COLLECTOR_PRIVATE_KEY found in environment (${#LOCAL_COLLECTOR_PRIVATE_KEY} characters)"

    # Validate key format (128 hex characters)
    if [ ${#LOCAL_COLLECTOR_PRIVATE_KEY} -eq 128 ]; then
        echo "✅ P2P key format is valid (128 hex characters)"
    else
        echo "❌ P2P key format is invalid: ${#LOCAL_COLLECTOR_PRIVATE_KEY} characters (expected 128)"
        echo "   Please check your LOCAL_COLLECTOR_PRIVATE_KEY environment variable"
        exit 1
    fi
else
    echo "❌ LOCAL_COLLECTOR_PRIVATE_KEY not found in environment"
    echo "   Please run './scripts/setup_operator_keys.sh' to generate a P2P key"
    exit 1
fi

echo "🚀 Starting Local Collector..."
/usr/local/bin/snapshotter-local-collector 