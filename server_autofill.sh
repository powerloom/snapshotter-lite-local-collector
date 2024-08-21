#!/bin/bash

set -e

echo 'populating server settings from environment values...';
export LOCAL_COLLECTOR_PORT="${LOCAL_COLLECTOR_PORT:-50051}"

if [ -z "$LOCAL_COLLECTOR_PORT" ]; then
    echo "LOCAL_COLLECTOR_PORT not found, please set this in your .env!";
    exit 1;
fi
if [ -z "$DATA_MARKET_CONTRACT" ]; then
    echo "DATA_MARKET_CONTRACT not found, please set this in your .env!";
    exit 1;
fi

cd config

# Template to actual settings.json manipulation
cp settings.example.json settings.json

priv_key="/keys/key.txt"

if [[ -f "$priv_key" ]]; then
    RELAYER_PRIVATE_KEY=$(cat "$priv_key")
else
    RELAYER_PRIVATE_KEY=""
fi

export RELAYER_PRIVATE_KEY

# Replace placeholders in settings.json with actual values from environment variables
sed -i'.backup' -e "s#POWERLOOM_REPORTING_URL#$POWERLOOM_REPORTING_URL#" \
                -e "s#SIGNER_ACCOUNT_ADDRESS#$SIGNER_ACCOUNT_ADDRESS#" \
                -e "s#LOCAL_COLLECTOR_PORT#$LOCAL_COLLECTOR_PORT#" \
                -e "s#RELAYER_PRIVATE_KEY#$RELAYER_PRIVATE_KEY#" settings.json \
                -e "s#SEQUENCER_MULTIADDR#$SEQUENCER_MULTIADDR#" \
                -e "s#TRUSTED_RELAYERS_LIST_URL#$TRUSTED_RELAYERS_LIST_URL#" settings.json \
                -e "s#DATA_MARKET_CONTRACT#$DATA_MARKET_CONTRACT#" settings.json

# Cleanup backup file
rm settings.json.backup
