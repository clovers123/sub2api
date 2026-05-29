#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BACKEND_PORT="${BACKEND_PORT:-9004}"
FRONTEND_PORT="${FRONTEND_PORT:-9005}"
TMP_DIR="$ROOT_DIR/tmp"
BACKEND_LOG="$TMP_DIR/sub2api-backend.log"
FRONTEND_LOG="$TMP_DIR/sub2api-frontend.log"
BACKEND_PID_FILE="$TMP_DIR/sub2api-backend.pid"
FRONTEND_PID_FILE="$TMP_DIR/sub2api-frontend.pid"
BACKEND_PID=""
FRONTEND_PID=""

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Missing command: $1" >&2
    exit 1
  fi
}

port_in_use() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

pid_is_running() {
  local pid="$1"
  [[ -n "$pid" ]] && kill -0 "$pid" >/dev/null 2>&1
}

read_pid_file() {
  local file="$1"
  if [[ -f "$file" ]]; then
    tr -d '[:space:]' < "$file"
  fi
}

stop_pid() {
  local pid="$1"
  local label="$2"

  if ! pid_is_running "$pid"; then
    return 0
  fi

  echo "Stopping $label ($pid)..."
  kill "$pid" >/dev/null 2>&1 || true

  for _ in $(seq 1 20); do
    if ! pid_is_running "$pid"; then
      return 0
    fi
    sleep 0.2
  done
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM

  stop_pid "$FRONTEND_PID" "frontend"
  stop_pid "$BACKEND_PID" "backend"
  rm -f "$FRONTEND_PID_FILE" "$BACKEND_PID_FILE"

  if [[ "$status" -eq 130 || "$status" -eq 143 ]]; then
    status=0
  fi

  exit "$status"
}

wait_for_port() {
  local port="$1"
  local label="$2"
  local timeout="${3:-60}"

  for _ in $(seq 1 "$timeout"); do
    if port_in_use "$port"; then
      echo "$label is listening on port $port"
      return 0
    fi
    sleep 1
  done

  echo "$label did not start on port $port within ${timeout}s" >&2
  return 1
}

ensure_no_stale_pid() {
  local file="$1"
  local label="$2"
  local pid

  pid="$(read_pid_file "$file")"
  if [[ -z "$pid" ]]; then
    rm -f "$file"
    return 0
  fi

  if pid_is_running "$pid"; then
    echo "$label already appears to be running from $file (pid $pid)." >&2
    echo "Run 'make local-stop' first, or stop that process manually." >&2
    exit 1
  fi

  rm -f "$file"
}

require_command lsof

if [[ ! -f "$ROOT_DIR/config.yaml" && ! -f "$ROOT_DIR/backend/config.yaml" ]]; then
  echo "Missing config.yaml. Put your backend config at $ROOT_DIR/config.yaml or $ROOT_DIR/backend/config.yaml." >&2
  exit 1
fi

if [[ ! -d "$ROOT_DIR/frontend/node_modules" ]]; then
  echo "Missing frontend/node_modules. Run: pnpm --dir frontend install --frozen-lockfile" >&2
  exit 1
fi

if [[ ! -x "$ROOT_DIR/frontend/node_modules/.bin/vite" ]]; then
  echo "Missing Vite binary. Run: pnpm --dir frontend install --frozen-lockfile" >&2
  exit 1
fi

mkdir -p "$TMP_DIR"
ensure_no_stale_pid "$BACKEND_PID_FILE" "Backend"
ensure_no_stale_pid "$FRONTEND_PID_FILE" "Frontend"

if port_in_use "$BACKEND_PORT"; then
  echo "Port $BACKEND_PORT is already in use. Stop that process first or set BACKEND_PORT." >&2
  exit 1
fi

if port_in_use "$FRONTEND_PORT"; then
  echo "Port $FRONTEND_PORT is already in use. Stop that process first or set FRONTEND_PORT." >&2
  exit 1
fi

: > "$BACKEND_LOG"
: > "$FRONTEND_LOG"

echo "Building backend..."
make -C "$ROOT_DIR/backend" build

trap cleanup EXIT INT TERM

(
  cd "$ROOT_DIR"
  exec env SERVER_PORT="$BACKEND_PORT" backend/bin/server >> "$BACKEND_LOG" 2>&1
) &
BACKEND_PID=$!
echo "$BACKEND_PID" > "$BACKEND_PID_FILE"
wait_for_port "$BACKEND_PORT" "Backend"

(
  cd "$ROOT_DIR/frontend"
  exec env \
    VITE_DEV_PORT="$FRONTEND_PORT" \
    VITE_DEV_PROXY_TARGET="http://localhost:$BACKEND_PORT" \
    node_modules/.bin/vite --config vite.config.ts --host 0.0.0.0 >> "$FRONTEND_LOG" 2>&1
) &
FRONTEND_PID=$!
echo "$FRONTEND_PID" > "$FRONTEND_PID_FILE"
wait_for_port "$FRONTEND_PORT" "Frontend"

echo
echo "Sub2API is running:"
echo "  Frontend: http://localhost:$FRONTEND_PORT/"
echo "  Backend:  http://localhost:$BACKEND_PORT"
echo
echo "Logs:"
echo "  tail -f tmp/sub2api-backend.log"
echo "  tail -f tmp/sub2api-frontend.log"
echo
echo "Press Ctrl-C here to stop both services."

while true; do
  if ! pid_is_running "$BACKEND_PID"; then
    echo "Backend exited. Last log lines:" >&2
    tail -60 "$BACKEND_LOG" >&2 || true
    exit 1
  fi

  if ! pid_is_running "$FRONTEND_PID"; then
    echo "Frontend exited. Last log lines:" >&2
    tail -60 "$FRONTEND_LOG" >&2 || true
    exit 1
  fi

  sleep 2
done
