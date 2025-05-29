#!/bin/bash

handle_exit() {
    EXIT_CODE=$?
    
    echo "Container exited with code $EXIT_CODE. Restarting..."
    exit 1
}

trap 'handle_exit' EXIT HUP INT QUIT ABRT TERM

echo 'starting pm2...';

pm2 start pm2.config.js

pm2 logs --lines 1000
