#!/usr/bin/env bash
set -euo pipefail

PORT=3001
EXECUTABLE_NAME="transwork-server.local"
KILL_PORT_OWNER=0

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
    -k|--kill-port-owner)
      KILL_PORT_OWNER=1
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
EXECUTABLE_PATH="$REPO_ROOT/$EXECUTABLE_NAME"
STOPPED_ANY=0

if pgrep -f "$EXECUTABLE_PATH" >/dev/null 2>&1; then
  pkill -f "$EXECUTABLE_PATH"
  echo "Stopped $EXECUTABLE_NAME"
  STOPPED_ANY=1
fi

PORT_OWNER=""
if command -v lsof >/dev/null 2>&1; then
  PORT_OWNER="$(lsof -ti tcp:"$PORT" -sTCP:LISTEN 2>/dev/null | head -n 1 || true)"
elif command -v ss >/dev/null 2>&1; then
  PORT_OWNER="$(ss -ltnp "sport = :$PORT" 2>/dev/null | awk 'NR>1 {print $NF}' | sed -n 's/.*pid=\([0-9]\+\).*/\1/p' | head -n 1)"
fi

if [[ -n "$PORT_OWNER" ]]; then
  if [[ "$KILL_PORT_OWNER" -eq 1 ]]; then
    kill "$PORT_OWNER"
    echo "Stopped process on port $PORT (PID $PORT_OWNER)"
    STOPPED_ANY=1
  elif [[ "$STOPPED_ANY" -eq 0 ]]; then
    echo "Port $PORT is still owned by PID $PORT_OWNER."
    echo "Re-run with --kill-port-owner if you want this script to terminate that process."
  fi
elif [[ "$STOPPED_ANY" -eq 0 ]]; then
  echo "No local Gressio server process found."
fi
