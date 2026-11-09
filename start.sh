#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

if [ ! -f ./antiapi ]; then
  echo "==> Building antiapi..."
  go build -o antiapi .
fi

exec ./antiapi "$@"
