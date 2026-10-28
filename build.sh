#!/bin/sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
cd "$ROOT"
mkdir -p bin
VERSION=$(git rev-parse --short HEAD 2>/dev/null || printf dev)
export CGO_ENABLED=0
export GOTOOLCHAIN=go1.27.3
go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o bin/mcp-gateway ./src/cmd/mcp-gateway
printf 'Built %s\n' "$ROOT/bin/mcp-gateway"
