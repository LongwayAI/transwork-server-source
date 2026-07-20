#!/usr/bin/env bash
set -euo pipefail

PORT=3001
EXECUTABLE_NAME="transwork-server.local"
BUILD=0
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
    -b|--build)
      BUILD=1
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
EXECUTABLE_PATH="$REPO_ROOT/$EXECUTABLE_NAME"
OUT_LOG="$REPO_ROOT/transwork-server.local.out.log"
ERR_LOG="$REPO_ROOT/transwork-server.local.err.log"

if [[ "$BUILD" -eq 1 || ! -f "$EXECUTABLE_PATH" ]]; then
  (cd "$REPO_ROOT" && go build -o "$EXECUTABLE_NAME" .)
fi

if pgrep -f "$EXECUTABLE_PATH" >/dev/null 2>&1; then
  echo "A '$EXECUTABLE_NAME' process is already running. Stop it first with transwork/scripts/stop-local.sh." >&2
  exit 1
fi

cd "$REPO_ROOT"
if [[ "$FOREGROUND" -eq 1 ]]; then
  PORT="$PORT" exec "$EXECUTABLE_PATH" --port "$PORT"
else
  nohup "$EXECUTABLE_PATH" --port "$PORT" >"$OUT_LOG" 2>"$ERR_LOG" &
  PID=$!
  echo "Started $EXECUTABLE_NAME (PID $PID) on http://127.0.0.1:$PORT"
  echo "stdout: $OUT_LOG"
  echo "stderr: $ERR_LOG"
fi
