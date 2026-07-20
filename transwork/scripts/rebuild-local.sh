#!/usr/bin/env bash
set -euo pipefail

PORT=3001
EXECUTABLE_NAME="transwork-server.local"
START_AFTER_BUILD=0
FOREGROUND=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -p|--port)
      PORT="$2"
      shift 2
      ;;
    -e|--executable)
      EXECUTABLE_NAME="$2"
      shift 2
      ;;
    -s|--start)
      START_AFTER_BUILD=1
      shift
      ;;
    -f|--foreground)
      FOREGROUND=1
      shift
      ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

"$SCRIPT_DIR/stop-local.sh" --port "$PORT" --executable "$EXECUTABLE_NAME"
(cd "$REPO_ROOT" && go build -o "$EXECUTABLE_NAME" .)

echo "Built $EXECUTABLE_NAME"

if [[ "$START_AFTER_BUILD" -eq 1 ]]; then
  if [[ "$FOREGROUND" -eq 1 ]]; then
    exec "$SCRIPT_DIR/start-local.sh" --port "$PORT" --executable "$EXECUTABLE_NAME" --foreground
  else
    "$SCRIPT_DIR/start-local.sh" --port "$PORT" --executable "$EXECUTABLE_NAME"
  fi
fi
