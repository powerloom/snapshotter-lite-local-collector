#!/bin/sh

handle_exit() {
    EXIT_CODE=$?
    echo "Local collector container exited with code $EXIT_CODE. Restarting..."
    exit 1
}

trap 'handle_exit' EXIT HUP INT QUIT ABRT TERM

/usr/local/bin/snapshotter-local-collector 