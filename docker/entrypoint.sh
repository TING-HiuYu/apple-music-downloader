#!/bin/sh
set -eu
umask 077

unset USERNAME PASSWORD

if [ "${1:-}" = "--web" ]; then
  shift
  set -- --web --listen "${AMDL_LISTEN:-0.0.0.0:8080}" "$@"
fi

exec apple-music-dl "$@"
