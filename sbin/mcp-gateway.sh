#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
BIN="$ROOT/bin/mcp-gateway"
case "${1:-status}" in
  start)
    "$BIN" --check-config
    mkdir -p "$ROOT/runtime/logs"
    chmod 700 "$ROOT/runtime" "$ROOT/runtime/logs"
    # Early startup diagnostics only; service and SDK logs rotate in Go.
    umask 077
    nohup "$BIN" </dev/null >"$ROOT/runtime/logs/startup.log" 2>&1 &
    PID=$!
    sleep 1
    if ! kill -0 "$PID" 2>/dev/null; then cat "$ROOT/runtime/logs/startup.log" >&2; exit 1; fi
    "$BIN" --control status
    ;;
  stop|status) "$BIN" --control "${1:-status}" ;;
  restart) "$BIN" --control stop; exec "$0" start ;;
  *) printf 'Usage: %s {start|stop|restart|status}\n' "$0" >&2; exit 2 ;;
esac
