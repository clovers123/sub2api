#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$ROOT_DIR/tmp"
BACKEND_PID_FILE="$TMP_DIR/sub2api-backend.pid"
FRONTEND_PID_FILE="$TMP_DIR/sub2api-frontend.pid"
BACKEND_PORT="${BACKEND_PORT:-9004}"
FRONTEND_PORT="${FRONTEND_PORT:-9005}"

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

stop_pid_file() {
  local file="$1"
  local label="$2"
  local pid

  pid="$(read_pid_file "$file")"
  if [[ -z "$pid" ]]; then
    echo "$label is not running"
    rm -f "$file"
    return 0
  fi

  if ! pid_is_running "$pid"; then
    echo "$label is not running"
    rm -f "$file"
    return 0
  fi

  echo "Stopping $label ($pid)..."
  kill "$pid" >/dev/null 2>&1 || true

  for _ in $(seq 1 20); do
    if ! pid_is_running "$pid"; then
      rm -f "$file"
      echo "Stopped $label"
      return 0
    fi
    sleep 0.2
  done

  echo "$label did not stop cleanly; pid $pid may still be running." >&2
  return 1
}

stop_pid_file "$FRONTEND_PID_FILE" "frontend"
stop_pid_file "$BACKEND_PID_FILE" "backend"

stop_port() {
  local port="$1"
  local label="$2"
  local pids

  if ! command -v lsof >/dev/null 2>&1; then
    return 0
  fi

  pids="$(lsof -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [[ -z "$pids" ]]; then
    return 0
  fi

  echo "Stopping $label listener on port $port ($pids)..."
  kill $pids >/dev/null 2>&1 || true
}

stop_port "$FRONTEND_PORT" "frontend"
stop_port "$BACKEND_PORT" "backend"
