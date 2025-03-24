#!/bin/bash

# Set the Victoria Metrics URL (change to match your environment)
export VICTORIAMETRICS_URL=${VICTORIAMETRICS_URL:-"http://35.190.140.72:8428"}

# Configure test parameters
TEST_NAME="your_test_name"
CLOUD="gce"
BRANCH="master"
UNIT="ops/s"
DAYS=90
BASELINE=45  # Default baseline is 45 days

echo "Setting up query for test=$TEST_NAME with $BASELINE days baseline comparison"

# First check what metrics are available for this test
echo "Checking available metrics for test $TEST_NAME..."
METRICS_URL="http://localhost:8080/dashboard/test_info.json?test=$TEST_NAME"
echo "Query URL: $METRICS_URL"

TEST_INFO=$(curl -s "$METRICS_URL")
echo "$TEST_INFO" | jq .

# Extract the first available metric name for this test
AVAILABLE_METRICS=$(echo "$TEST_INFO" | jq -r '.metrics[]' 2>/dev/null)
if [ -z "$AVAILABLE_METRICS" ]; then
    echo "No metrics found for test $TEST_NAME!"
    echo "Try using a different test name or check that the test exists in VictoriaMetrics."
    exit 1
fi

FIRST_METRIC=$(echo "$AVAILABLE_METRICS" | head -1)
echo "Found metric: $FIRST_METRIC"

# Get available units for this test
AVAILABLE_UNITS=$(echo "$TEST_INFO" | jq -r '.labels.unit[]' 2>/dev/null)
if [ -n "$AVAILABLE_UNITS" ]; then
    echo "Available units for $TEST_NAME:"
    echo "$AVAILABLE_UNITS"
    # Use the first available unit if one exists
    FIRST_UNIT=$(echo "$AVAILABLE_UNITS" | head -1)
    if [ -n "$FIRST_UNIT" ]; then
        UNIT=$FIRST_UNIT
        echo "Using unit: $UNIT"
    fi
fi

# Then run the query with baseline comparison and specific metric
QUERY_URL="http://localhost:8080/dashboard/data.json?benchmark=$TEST_NAME&unit=$UNIT&cloud=$CLOUD&branch=$BRANCH&days=$DAYS&baseline=$BASELINE"
echo "Running query: $QUERY_URL"
curl -s "$QUERY_URL" | jq .

echo "You can view the results in your browser at:"
echo "http://localhost:8080/dashboard/?benchmark=$TEST_NAME&unit=$UNIT&cloud=$CLOUD&branch=$BRANCH&days=$DAYS&baseline=$BASELINE"

echo ""
echo "Note: Baseline comparisons are calculated locally by the dashboard, not stored in VictoriaMetrics." 