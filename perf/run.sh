#!/bin/bash

# Exit on error
set -e

# Default VictoriaMetrics URL if not set
export VICTORIAMETRICS_URL=${VICTORIAMETRICS_URL:-"http://35.190.140.72:8428"}

# Default Grafana URL if not set
export GRAFANA_URL=${GRAFANA_URL:-"http://35.190.140.72:3000"}

# Clean any previous build
rm -f perf-server

# Build the program
go build -o perf-server

# Run the server
./perf-server -victoriametrics-url="$VICTORIAMETRICS_URL" -listen-http=:8080 -grafana-url="$GRAFANA_URL"