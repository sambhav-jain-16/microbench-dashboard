#!/bin/bash

# Set the Victoria Metrics URL (change to match your environment)
export VICTORIAMETRICS_URL=${VICTORIAMETRICS_URL:-"http://35.190.140.72:8428"}

# Configure test parameters
if [ "$1" != "" ]; then
  TEST_NAME="$1"
else
  # Default test name - replace with one from your system
  TEST_NAME="kv95"
fi

CLOUD="gce"
BRANCH="master"
DAYS=90
BASELINE=45  # Default baseline is 45 days

echo "Fetching benchmark data for test=$TEST_NAME using series_data.json endpoint"

# First let's check if the test exists and what metrics it has
TEST_INFO_URL="http://localhost:8080/dashboard/test_info.json?test=$TEST_NAME"
echo "Checking test info: $TEST_INFO_URL"

TEST_INFO=$(curl -s "$TEST_INFO_URL")
METRICS_COUNT=$(echo "$TEST_INFO" | jq -r '.metrics_count')

if [ "$METRICS_COUNT" == "" ] || [ "$METRICS_COUNT" == "null" ] || [ "$METRICS_COUNT" -eq 0 ]; then
  echo "No metrics found for test $TEST_NAME. Available tests:"
  curl -s "http://localhost:8080/dashboard/tests.json" | jq -r '.tests[] | select(length > 0)' | head -10
  exit 1
fi

echo "Found $METRICS_COUNT metrics for test $TEST_NAME"
echo "Available metrics:"
echo "$TEST_INFO" | jq -r '.metrics[]'
echo "Available units:"
echo "$TEST_INFO" | jq -r '.labels.unit[]'

# Now fetch the series data converted to BenchmarkJSON format
SERIES_URL="http://localhost:8080/dashboard/series_data.json?test=$TEST_NAME&cloud=$CLOUD&branch=$BRANCH&days=$DAYS"
echo -e "\nFetching benchmark data: $SERIES_URL"

# Fetch and display the benchmark data
BENCHMARK_DATA=$(curl -s "$SERIES_URL")
BENCHMARKS_COUNT=$(echo "$BENCHMARK_DATA" | jq -r '.benchmarks | length')

if [ "$BENCHMARKS_COUNT" == "" ] || [ "$BENCHMARKS_COUNT" == "null" ] || [ "$BENCHMARKS_COUNT" -eq 0 ]; then
  echo "No benchmark data found for test $TEST_NAME"
  exit 1
fi

echo "Found $BENCHMARKS_COUNT benchmark series"

# Display summary of each benchmark
echo -e "\nBenchmark Series Summary:"
echo "$BENCHMARK_DATA" | jq -r '.benchmarks[] | "Name: \(.Name), Unit: \(.Unit), Values: \(.Values | length)"'

# Show some sample values from the first benchmark
echo -e "\nSample values from first benchmark:"
echo "$BENCHMARK_DATA" | jq -r '.benchmarks[0].Values[:5] | .[] | "Date: \(.CommitDate), Value: \(.Center)"'

echo -e "\nYou can view these benchmarks in the dashboard at:"
echo "http://localhost:8080/dashboard/?benchmark=$TEST_NAME&cloud=$CLOUD&branch=$BRANCH&days=$DAYS&baseline=$BASELINE" 