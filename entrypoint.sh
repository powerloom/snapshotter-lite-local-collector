#!/bin/sh
set -a
[ -f /.env ] && . /.env
set +a
exec "$@"
