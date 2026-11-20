#!/bin/sh
set -eu
# Paths are siblings: shared music is /music/public, user libraries /music/users.
# Navidrome validates/creates both at startup; do not mount /music as a library.
mkdir -p /data
exec /app/navidrome "$@"
