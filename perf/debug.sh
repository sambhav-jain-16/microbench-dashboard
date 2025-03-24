#!/bin/bash

# Exit on error
set -e

# Default VictoriaMetrics URL if not set
export VICTORIAMETRICS_URL=${VICTORIAMETRICS_URL:-"http://35.190.140.72:8428"}

# Install Delve if it's not already installed
if ! command -v dlv &> /dev/null; then
    echo "Installing Delve debugger..."
    go install github.com/go-delve/delve/cmd/dlv@latest
fi

# Build the program with debug information
echo "Building with debug symbols..."
go build -gcflags="all=-N -l" -o perf-server-debug 

# Run the debugger
echo "Starting debugger..."
dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./perf-server-debug -- -victoriametrics-url="$VICTORIAMETRICS_URL" -listen-http=:8080 