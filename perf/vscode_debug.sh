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

echo "Starting Delve in headless mode..."
echo "Now you can connect using the 'Attach to Delve' configuration in VS Code"
echo "Press Ctrl+C to stop the debugger"

cd "$(dirname "$0")"
dlv debug --headless --listen=:2345 --api-version=2 --accept-multiclient -- -victoriametrics-url="$VICTORIAMETRICS_URL" -listen-http=:8080 