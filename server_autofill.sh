#!/bin/bash

set -e

echo 'populating server settings from environment values...';

if [ -z "$RELAYER_URL" ]; then
    echo "RELAYER_URL not found, please set this in your .env!";
    exit 1;
fi

if [ -z "$RELAYER_ID" ]; then
    echo "RELAYER_ID not found, please set this in your .env!";
    exit 1;
fi

if [ -z "$COLLECTOR_ID" ]; then
    echo "COLLECTOR_ID not found, please set this in your .env!";
    exit 1;
fi

cd config

# Template to actual settings.json manipulation
cp settings.example.json settings.json

# Replace placeholders in settings.json with actual values from environment variables
sed -i'.backup' -e "s#COLLECTOR_ID#$COLLECTOR_ID#" \
                -e "s#RELAYER_ID#$RELAYER_ID#" \
                -e "s#RELAYER_URL#$RELAYER_URL#"

# Cleanup backup file
rm settings.json.backup
