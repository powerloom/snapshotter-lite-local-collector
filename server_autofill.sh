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

if [ -z "$DATA_MARKET_IN_REQUEST" ]; then
    # set default to false
    echo "DATA_MARKET_IN_REQUEST not found, setting to false";
    DATA_MARKET_IN_REQUEST="false"
else
    echo "DATA_MARKET_IN_REQUEST found, setting to $DATA_MARKET_IN_REQUEST";
fi

export MAX_STREAM_POOL_SIZE="${MAX_STREAM_POOL_SIZE:-2}"
export STREAM_POOL_HEALTH_CHECK_INTERVAL="${STREAM_POOL_HEALTH_CHECK_INTERVAL:-30}"
export ENABLE_CRON_RESTART_LOCAL_COLLECTOR="${ENABLE_CRON_RESTART_LOCAL_COLLECTOR:-false}"

priv_key="/keys/key.txt"

if [[ -f "$priv_key" ]]; then
    RELAYER_PRIVATE_KEY=$(cat "$priv_key")
else
    RELAYER_PRIVATE_KEY=""
fi

export RELAYER_PRIVATE_KEY
