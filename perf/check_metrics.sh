#!/bin/bash

# Set the Victoria Metrics URL (change to match your environment)
export VICTORIAMETRICS_URL=${VICTORIAMETRICS_URL:-"http://35.190.140.72:8428"}

echo "Victoria Metrics URL: $VICTORIAMETRICS_URL"

# Get all metric names
echo "Fetching list of all available metrics..."
METRICS_NAMES=$(curl -s "$VICTORIAMETRICS_URL/api/v1/label/__name__/values" | jq -r '.data[]')

# Check if we got any metrics
if [ -z "$METRICS_NAMES" ]; then
    echo "Error: Could not retrieve metrics from $VICTORIAMETRICS_URL"
    echo "Please check if the URL is correct and the server is running."
    exit 1
fi

METRIC_COUNT=$(echo "$METRICS_NAMES" | wc -l)
echo "Found $METRIC_COUNT metrics."
echo "First 10 metrics:"
echo "$METRICS_NAMES" | head -10

# Get all test names
echo -e "\nFetching list of all available tests..."
TEST_NAMES=$(curl -s "$VICTORIAMETRICS_URL/api/v1/label/test/values" | jq -r '.data[]')

# Check if we got any tests
if [ -z "$TEST_NAMES" ]; then
    echo "Warning: No 'test' labels found in metrics. Your metrics may use a different label structure."
else
    TEST_COUNT=$(echo "$TEST_NAMES" | wc -l)
    echo "Found $TEST_COUNT tests."
    echo "First 10 tests:"
    echo "$TEST_NAMES" | head -10
fi

# Function to check metrics for a specific test
check_test_metrics() {
    local test_name="$1"
    echo -e "\nChecking metrics for test: $test_name"
    
    # Construct query for series matching this test
    SERIES_URL="$VICTORIAMETRICS_URL/api/v1/series?match[]={test=\"$test_name\"}"
    SERIES_DATA=$(curl -s "$SERIES_URL")
    
    # Extract the metrics that have this test label
    TEST_METRICS=$(echo "$SERIES_DATA" | jq -r '.data[] | ."__name__"' | sort | uniq)
    
    if [ -z "$TEST_METRICS" ]; then
        echo "No metrics found for test: $test_name"
    else
        METRIC_COUNT=$(echo "$TEST_METRICS" | wc -l)
        echo "Found $METRIC_COUNT metric(s) for test: $test_name"
        echo "$TEST_METRICS"
        
        # Extract all labels for the first metric
        FIRST_METRIC=$(echo "$TEST_METRICS" | head -1)
        if [ -n "$FIRST_METRIC" ]; then
            echo -e "\nLabel values for metric '$FIRST_METRIC' with test='$test_name':"
            echo "$SERIES_DATA" | jq -r ".data[] | select(.__name__ == \"$FIRST_METRIC\" and .test == \"$test_name\") | to_entries | map(\"\(.key)=\(.value)\") | join(\", \")" | head -5
            
            echo -e "\nSample query for this metric:"
            echo "$VICTORIAMETRICS_URL/api/v1/query_range?query=$FIRST_METRIC{test=\"$test_name\"}&start=$(date -u -d "1 day ago" +%s)&end=$(date -u +%s)&step=1h"
        fi
    fi
}

# Ask user if they want to check a specific test
echo -e "\nDo you want to check metrics for a specific test? (y/n)"
read -r response
if [[ "$response" =~ ^[Yy] ]]; then
    echo "Enter test name (or leave empty to use first test):"
    read -r test_name
    
    if [ -z "$test_name" ] && [ -n "$TEST_NAMES" ]; then
        test_name=$(echo "$TEST_NAMES" | head -1)
        echo "Using first available test: $test_name"
    fi
    
    if [ -n "$test_name" ]; then
        check_test_metrics "$test_name"
    else
        echo "No test name provided and no tests available."
    fi
fi

echo -e "\nDone. You can use the dashboard at http://localhost:8080/dashboard/ to explore these metrics." 