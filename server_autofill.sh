#!/bin/bash

set -e

echo 'populating server settings from environment values...';

if [ -z "$CLIENT_RENDEZVOUS_POINT" ]; then
    echo "CLIENT_RENDEZVOUS_POINT not found, please set this in your .env!";
    exit 1;
fi
if [ -z "$RELAYER_RENDEZVOUS_POINT" ]; then
    echo "RELAYER_RENDEZVOUS_POINT not found, please set this in your .env!";
    exit 1;
fi
if [ -z "$SEQUENCER_ID" ]; then
    echo "SEQUENCER_ID not found, please set this in your .env!";
    exit 1;
fi

cd config

# Template to actual settings.json manipulation
cp settings.example.json settings.json

RELAYER_PRIVATE_KEY=$(cat /config/key.txt)

# Replace placeholders in settings.json with actual values from environment variables
sed -i'.backup' -e "s#SEQUENCER_ID#$SEQUENCER_ID#" \
                -e "s#RELAYER_RENDEZVOUS_POINT#$RELAYER_RENDEZVOUS_POINT#" \
                -e "s#CLIENT_RENDEZVOUS_POINT#$CLIENT_RENDEZVOUS_POINT#" \
                -e "s#DASHBOARD_ENABLED#$DASHBOARD_ENABLED#" \
                -e "s#RELAYER_PRIVATE_KEY#$RELAYER_PRIVATE_KEY#" settings.json

# Cleanup backup file
rm settings.json.backup
