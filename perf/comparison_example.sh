#!/bin/bash

# This script demonstrates how to use the benchmark comparison feature in the dashboard

# Set the Victoria Metrics URL (change to match your environment)
VICTORIAMETRICS_URL=${VICTORIAMETRICS_URL:-"http://35.190.140.72:8428"}
DASHBOARD_URL="http://localhost:8080"

# Configure test parameters
if [ "$1" != "" ]; then
  TEST_NAME="$1"
else
  # Default test name - replace with one from your system
  TEST_NAME="kv95"
fi

CLOUD="gce"
BRANCH="master"
DAYS=30
BASELINE=45  # Days to use as baseline

echo "Showing benchmark comparisons for test=$TEST_NAME with $DAYS days of data and $BASELINE days of baseline"

# Let's check available metrics in VictoriaMetrics first
echo "Checking available metrics in VictoriaMetrics..."
METRICS_URL="${VICTORIAMETRICS_URL}/api/v1/label/__name__/values"
METRICS=$(curl -s "$METRICS_URL")
METRICS_COUNT=$(echo "$METRICS" | jq -r '.data | length')

if [ "$METRICS_COUNT" -eq 0 ]; then
  echo "No metrics found in VictoriaMetrics at $VICTORIAMETRICS_URL"
  exit 1
fi

echo "Found $METRICS_COUNT metrics in VictoriaMetrics"
SAMPLE_METRICS=$(echo "$METRICS" | jq -r '.data | .[0:5]')
echo "Sample metrics: $SAMPLE_METRICS"

# Check for available tests
echo "Checking available tests..."
TESTS_URL="${VICTORIAMETRICS_URL}/api/v1/label/test/values"
TESTS=$(curl -s "$TESTS_URL")
TESTS_COUNT=$(echo "$TESTS" | jq -r '.data | length')

if [ "$TESTS_COUNT" -eq 0 ]; then
  echo "No tests found in VictoriaMetrics. Check if your data has the 'test' label."
  echo "Available labels in VictoriaMetrics:"
  LABELS_URL="${VICTORIAMETRICS_URL}/api/v1/labels"
  curl -s "$LABELS_URL" | jq -r '.data | .[0:10]'
  exit 1
fi

echo "Found $TESTS_COUNT tests in VictoriaMetrics"
echo "Available tests (first 10):"
echo "$TESTS" | jq -r '.data | .[0:10]'

# Now check if our specific test exists
echo "Checking if test '$TEST_NAME' exists..."
TEST_EXISTS=$(echo "$TESTS" | jq -r '.data | index("'"$TEST_NAME"'")')

if [ "$TEST_EXISTS" == "null" ]; then
  echo "Test '$TEST_NAME' not found in VictoriaMetrics. Available tests (first 10):"
  echo "$TESTS" | jq -r '.data | .[0:10]'
  echo "Please choose one of the available tests or check your data."
  exit 1
fi

echo "Test '$TEST_NAME' found in VictoriaMetrics"

# Check for series data for this test
SERIES_URL="${VICTORIAMETRICS_URL}/api/v1/series?match[]={test=\"$TEST_NAME\",cloud=\"$CLOUD\",branch=\"$BRANCH\"}"
SERIES=$(curl -s "$SERIES_URL")
SERIES_COUNT=$(echo "$SERIES" | jq -r '.data | length')

if [ "$SERIES_COUNT" -eq 0 ]; then
  echo "No series found for test='$TEST_NAME', cloud='$CLOUD', branch='$BRANCH'"
  
  # Try with just the test name to see if it exists with different labels
  SIMPLE_URL="${VICTORIAMETRICS_URL}/api/v1/series?match[]={test=\"$TEST_NAME\"}"
  SIMPLE_SERIES=$(curl -s "$SIMPLE_URL")
  SIMPLE_COUNT=$(echo "$SIMPLE_SERIES" | jq -r '.data | length')
  
  if [ "$SIMPLE_COUNT" -gt 0 ]; then
    echo "Found $SIMPLE_COUNT series with test='$TEST_NAME' but with different labels."
    echo "Sample series for this test:"
    echo "$SIMPLE_SERIES" | jq -r '.data | .[0:3]'
    echo "Try adjusting cloud/branch parameters accordingly."
  else
    echo "No series found for test='$TEST_NAME' with any labels. Check your test name."
  fi
  
  exit 1
fi

echo "Found $SERIES_COUNT series for test='$TEST_NAME', cloud='$CLOUD', branch='$BRANCH'"
echo "Sample series:"
echo "$SERIES" | jq -r '.data | .[0:3]'

# Check for values in the current time range
CURRENT_START=$(date -v-${DAYS}d +%s)
CURRENT_END=$(date +%s)
QUERY_URL="${VICTORIAMETRICS_URL}/api/v1/query_range?query={test=\"$TEST_NAME\",cloud=\"$CLOUD\",branch=\"$BRANCH\"}&start=$CURRENT_START&end=$CURRENT_END&step=3600"

echo "Checking for data points in the current range ($DAYS days)..."
QUERY_RESULT=$(curl -s "$QUERY_URL")
RESULT_STATUS=$(echo "$QUERY_RESULT" | jq -r '.status')

if [ "$RESULT_STATUS" != "success" ]; then
  echo "Error querying VictoriaMetrics: $(echo "$QUERY_RESULT" | jq -r '.error')"
  exit 1
fi

RESULT_COUNT=$(echo "$QUERY_RESULT" | jq -r '.data.result | length')

if [ "$RESULT_COUNT" -eq 0 ]; then
  echo "No data points found for the current time range. Check if your data has values for the last $DAYS days."
  exit 1
fi

echo "Found $RESULT_COUNT result series with data points in the current time range"

# Check for values in the baseline time range
BASELINE_START=$(date -v-${DAYS}d -v-${BASELINE}d +%s)
BASELINE_END=$(date -v-${DAYS}d +%s)
BASELINE_URL="${VICTORIAMETRICS_URL}/api/v1/query_range?query={test=\"$TEST_NAME\",cloud=\"$CLOUD\",branch=\"$BRANCH\"}&start=$BASELINE_START&end=$BASELINE_END&step=3600"

echo "Checking for data points in the baseline range ($BASELINE days before current)..."
BASELINE_RESULT=$(curl -s "$BASELINE_URL")
BASELINE_STATUS=$(echo "$BASELINE_RESULT" | jq -r '.status')

if [ "$BASELINE_STATUS" != "success" ]; then
  echo "Error querying VictoriaMetrics for baseline: $(echo "$BASELINE_RESULT" | jq -r '.error')"
  exit 1
fi

BASELINE_COUNT=$(echo "$BASELINE_RESULT" | jq -r '.data.result | length')

if [ "$BASELINE_COUNT" -eq 0 ]; then
  echo "No data points found for the baseline time range. Check if your data has values from $BASELINE days before the current range."
  echo "The comparison will still work but won't be able to show delta from baseline."
fi

# First let's check if the test info endpoint can see this test
TEST_INFO_URL="$DASHBOARD_URL/dashboard/test_info.json?test=$TEST_NAME"
echo "Checking test info from dashboard: $TEST_INFO_URL"

TEST_INFO=$(curl -s "$TEST_INFO_URL")
METRICS_COUNT=$(echo "$TEST_INFO" | jq -r '.metrics_count')

if [ "$METRICS_COUNT" == "" ] || [ "$METRICS_COUNT" == "null" ] || [ "$METRICS_COUNT" -eq 0 ]; then
  echo "No metrics found for test $TEST_NAME in the dashboard. Available tests:"
  curl -s "$DASHBOARD_URL/dashboard/tests.json" | jq -r '.tests[] | select(length > 0)' | sort | head -10
  echo "This may indicate the dashboard can't connect to VictoriaMetrics or the test doesn't exist."
  exit 1
fi

echo "Dashboard found $METRICS_COUNT metrics for test $TEST_NAME"
echo "Available metrics from dashboard:"
echo "$TEST_INFO" | jq -r '.metrics[]'

# Now access the dashboard with the benchmark comparisons
DASHBOARD_LINK="$DASHBOARD_URL/dashboard/?benchmark=$TEST_NAME&cloud=$CLOUD&branch=$BRANCH&days=$DAYS&baseline=$BASELINE"
echo -e "\nView the benchmark comparisons at:"
echo "$DASHBOARD_LINK"

# Optionally, fetch the data directly for inspection
DATA_URL="$DASHBOARD_URL/dashboard/data.json?benchmark=$TEST_NAME&cloud=$CLOUD&branch=$BRANCH&days=$DAYS&baseline=$BASELINE"
echo -e "\nDo you want to fetch and examine the raw benchmark data? (y/n)"
read answer

if [[ "$answer" == "y" || "$answer" == "Y" ]]; then
  echo "Fetching data from: $DATA_URL"
  BENCHMARK_DATA=$(curl -s "$DATA_URL")
  
  # Check for successful response
  if [[ "$BENCHMARK_DATA" =~ "No benchmarks found" ]]; then
    echo "Error: No benchmarks found. Raw response:"
    echo "$BENCHMARK_DATA"
    exit 1
  fi
  
  # Show benchmark names and comparison results
  BENCHMARK_COUNT=$(echo "$BENCHMARK_DATA" | jq -r '.Benchmarks | length')
  
  if [ "$BENCHMARK_COUNT" -eq 0 ]; then
    echo "No benchmarks found in the response."
    echo "Raw response:"
    echo "$BENCHMARK_DATA"
    exit 1
  fi
  
  echo -e "\nFound $BENCHMARK_COUNT benchmarks in the response"
  echo -e "\nBenchmark comparison results:"
  echo "$BENCHMARK_DATA" | jq -r '.Benchmarks[] | "Name: \(.Name), Unit: \(.Unit), Values: \(.Values | length)"'
  
  # Show details of the first benchmark comparison
  echo -e "\nFirst benchmark comparison details:"
  echo "$BENCHMARK_DATA" | jq -r '.Benchmarks[0].Values | .[] | "Date: \(.CommitDate), Change: \(.Center * 100)%"'
  
  # Summarize the comparison results
  echo -e "\nComparison Summary:"
  echo "$BENCHMARK_DATA" | jq -r '
    .Benchmarks[] | 
    (.Values | length) as $count |
    if $count > 0 then
      (.Values[0].Center * 100 | tostring) as $center |
      .Name + " (" + .Unit + "): " + $center + "% change compared to baseline"
    else
      .Name + " (" + .Unit + "): No values"
    end
  '
else
  echo "Skipping data fetch. Open the dashboard link to view the comparisons."
fi

echo -e "\nExplanation of comparison methodology:"
echo " - The benchmark uses golang.org/x/perf/benchfmt and benchseries for comparisons"
echo " - Current data is compared against a baseline period (default: $BASELINE days before current data)"
echo " - Results are shown as percentage changes from baseline"
echo " - For ops/s units, positive values indicate improvements, negative values indicate regressions"
echo " - For time-based units (ms, ns), positive values indicate regressions, negative values indicate improvements" 