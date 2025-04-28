// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/exp/maps"

	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/api/query"
	"golang.org/x/build/third_party/bandchart"
	"gopkg.in/yaml.v3"
)

// /dashboard/ displays a dashboard of benchmark results over time for
// performance monitoring.

//go:embed dashboard/*
var dashboardFS embed.FS

// TestsJSON is the response for the tests.json endpoint
type TestsJSON struct {
	Tests []string `json:"tests"`
}

// dashboardRegisterOnMux registers the dashboard URLs on mux.
func (a *App) dashboardRegisterOnMux(mux *http.ServeMux) {
	// Serve index.html as the default dashboard page
	mux.HandleFunc("/dashboard/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dashboard/" {
			// Always serve index.html which now includes the test list sidebar
			data, err := dashboardFS.ReadFile("dashboard/index.html")
			if err != nil {
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.Write(data)
			return
		}

		// For all other paths, serve from the dashboard filesystem
		http.FileServer(http.FS(dashboardFS)).ServeHTTP(w, r)
	})

	// Register other dashboard endpoints
	mux.Handle("/dashboard/third_party/bandchart/", http.StripPrefix("/dashboard/third_party/bandchart/", http.FileServer(http.FS(bandchart.FS))))
	mux.HandleFunc("/dashboard/data.json", a.dashboardData)
	mux.HandleFunc("/dashboard/formfields.json", a.formFields)
	mux.HandleFunc("/dashboard/tests.json", a.dashboardTests)
	mux.HandleFunc("/dashboard/metrics.json", a.listMetrics)
	mux.HandleFunc("/dashboard/test_info.json", a.testInfo)
	mux.HandleFunc("/dashboard/series_data.json", a.seriesDataToBenchmark)
	mux.HandleFunc("/dashboard/annotations.json", annotationsHandler)
}

// DataJSON is the result of accessing the data.json endpoint.
type DataJSON struct {
	Benchmarks []*BenchmarkJSON
	Commits    []Commit
}

// BenchmarkJSON contains the timeseries values for a single benchmark name +
// unit.
//
// We could try to shoehorn this into benchfmt.Result, but that isn't really
// the best fit for a graph.
type BenchmarkJSON struct {
	Name           string
	Unit           string
	HigherIsBetter bool

	// These will be sorted by CommitDate.
	Values []ValueJSON

	Regression *RegressionJSON
}

type ValueJSON struct {
	CommitHash           string
	CommitDate           time.Time
	BaselineCommitHash   string
	BaselineCommitDate   time.Time
	BenchmarksCommitHash string

	// These are pre-formatted as percent change.
	Low    float64
	Center float64
	High   float64
}

func fluxRecordToValue(rec *query.FluxRecord) (ValueJSON, error) {
	low, ok := rec.ValueByKey("low").(float64)
	if !ok {
		return ValueJSON{}, fmt.Errorf("record %s low value got type %T want float64", rec, rec.ValueByKey("low"))
	}

	center, ok := rec.ValueByKey("center").(float64)
	if !ok {
		return ValueJSON{}, fmt.Errorf("record %s center value got type %T want float64", rec, rec.ValueByKey("center"))
	}

	high, ok := rec.ValueByKey("high").(float64)
	if !ok {
		return ValueJSON{}, fmt.Errorf("record %s high value got type %T want float64", rec, rec.ValueByKey("high"))
	}

	commit, ok := rec.ValueByKey("experiment-commit").(string)
	if !ok {
		return ValueJSON{}, fmt.Errorf("record %s experiment-commit value got type %T want float64", rec, rec.ValueByKey("experiment-commit"))
	}

	baselineCommit, ok := rec.ValueByKey("baseline-commit").(string)
	if !ok {
		return ValueJSON{}, fmt.Errorf("record %s experiment-commit value got type %T want float64", rec, rec.ValueByKey("baseline-commit"))
	}

	benchmarksCommit, ok := rec.ValueByKey("benchmarks-commit").(string)
	if !ok {
		return ValueJSON{}, fmt.Errorf("record %s experiment-commit value got type %T want float64", rec, rec.ValueByKey("benchmarks-commit"))
	}

	return ValueJSON{
		CommitDate:           rec.Time(),
		CommitHash:           commit,
		BaselineCommitHash:   baselineCommit,
		BaselineCommitDate:   rec.Time(),
		BenchmarksCommitHash: benchmarksCommit,
		Low:                  low - 1,
		Center:               center - 1,
		High:                 high - 1,
	}, nil
}

// validateRe is an allowlist of characters for a PromQL string literal
var validateRe = regexp.MustCompile(`^[a-zA-Z0-9(),=/_:;.*-\[\]\\]*$`)

func validatePromQLString(s string) error {
	if !validateRe.MatchString(s) {
		return fmt.Errorf("malformed value %q", s)
	}
	return nil
}

var errBenchmarkNotFound = errors.New("benchmark not found")

func sanitizePromQLRegex(name string) string {
	return strings.Replace(name, "/", "\\/", -1)
}

type RegressionJSON struct {
	Change         float64 // endpoint regression, if any
	DeltaIndex     int     // index at which largest increase of regression occurs
	Delta          float64 // size of that changes
	IgnoredBecause string

	deltaScore float64 // score of that change (in 95%ile boxes)
}

// queryToJson process a QueryTableResult into a slice of BenchmarkJSON,
// with that slice in no particular order (i.e., it needs to be sorted or
// run-to-run results will vary).  For each benchmark in the slice, however,
// results are sorted into commit-date order.
func queryToJson(res *api.QueryTableResult) ([]*BenchmarkJSON, error) {
	type key struct {
		name string
		unit string
	}

	m := make(map[key]*BenchmarkJSON)

	for res.Next() {
		rec := res.Record()

		name, ok := rec.ValueByKey("name").(string)
		if !ok {
			return nil, fmt.Errorf("record %s name value got type %T want string", rec, rec.ValueByKey("name"))
		}

		unit, ok := rec.ValueByKey("unit").(string)
		if !ok {
			return nil, fmt.Errorf("record %s unit value got type %T want string", rec, rec.ValueByKey("unit"))
		}

		k := key{name, unit}
		b, ok := m[k]
		if !ok {
			b = &BenchmarkJSON{
				Name: name,
				Unit: unit,
			}
			m[k] = b
		}

		v, err := fluxRecordToValue(res.Record())
		if err != nil {
			return nil, err
		}

		b.Values = append(b.Values, v)
	}

	s := make([]*BenchmarkJSON, 0, len(m))
	for _, b := range m {
		// Ensure that the benchmarks are commit-date ordered.
		sort.Slice(b.Values, func(i, j int) bool {
			return b.Values[i].CommitDate.Before(b.Values[j].CommitDate)
		})
		s = append(s, b)
	}

	return s, nil
}

// filterAndSortRegressions filters out benchmarks that didn't regress and sorts the
// benchmarks in s so that those with the largest detectable regressions come first.
func filterAndSortRegressions(s []*BenchmarkJSON) []*BenchmarkJSON {
	// Compute per-benchmark estimates of point where the most interesting regression happened.
	for _, b := range s {
		b.Regression = worstRegression(b)
		// TODO(mknyszek, drchase, mpratt): Filter out benchmarks once we're confident this
		// algorithm works OK.
	}

	// Sort benchmarks with detectable regressions first, ordered by
	// size of regression at end of sample.  Also sort the remaining
	// benchmarks into end-of-sample regression order.
	sort.Slice(s, func(i, j int) bool {
		ri, rj := s[i].Regression, s[j].Regression
		// regressions w/ a delta index come first
		if (ri.DeltaIndex < 0) != (rj.DeltaIndex < 0) {
			return rj.DeltaIndex < 0
		}
		if ri.Change != rj.Change {
			// put larger regression first.
			return ri.Change > rj.Change
		}
		if s[i].Name == s[j].Name {
			return s[i].Unit < s[j].Unit
		}
		return s[i].Name < s[j].Name
	})
	return s
}

// groupBenchmarkResults groups all benchmark results from the passed query.
// if byRegression is true, order the benchmarks with largest current regressions
// with detectable points first.
func groupBenchmarkResults(res *api.QueryTableResult, byRegression bool) ([]*BenchmarkJSON, error) {
	s, err := queryToJson(res)
	if err != nil {
		return nil, err
	}
	if byRegression {
		return filterAndSortRegressions(s), nil
	}
	// Keep benchmarks with the same name grouped together, which is
	// assumed by the JS.
	sort.Slice(s, func(i, j int) bool {
		if s[i].Name == s[j].Name {
			return s[i].Unit < s[j].Unit
		}
		return s[i].Name < s[j].Name
	})
	return s, nil
}

// changeScore returns an indicator of the change and direction.
// This is a heuristic measure of the lack of overlap between
// two confidence intervals; minimum lack of overlap (i.e., same
// confidence intervals) is zero.  Exact non-overlap, meaning
// the high end of one interval is equal to the low end of the
// other, is one.  A gap of size G between the two intervals
// yields a score of 1 + G/M where M is the size of the larger
// interval (this suppresses changescores adjacent to noise).
// A partial overlap of size G yields a score of
// 1 - G/M.
//
// For point estimates (where low=center=high), the function
// returns a score based on the relative difference between
// the points, scaled to match the behavior of confidence intervals.
func changeScore(l1, c1, h1, l2, c2, h2 float64) float64 {
	sign := 1.0
	if c1 > c2 {
		l1, c1, h1, l2, c2, h2 = l2, c2, h2, l1, c1, h1
		sign = -sign
	}

	// Check if we're dealing with point estimates (low = center = high)
	isPoint1 := l1 == c1 && c1 == h1
	isPoint2 := l2 == c2 && c2 == h2

	if isPoint1 && isPoint2 {
		// Both are point estimates - calculate relative difference
		// Use a small epsilon to prevent division by zero
		epsilon := 1e-10
		// Use the larger of the absolute values as base, with minimum of epsilon
		base := math.Max(math.Max(math.Abs(c1), math.Abs(c2)), epsilon)
		relativeDiff := (c2 - c1) / base
		// Scale the relative difference to match confidence interval behavior
		// A 100% difference (c2 = 2*c1) should give a score of 1
		return sign * relativeDiff
	}

	// Calculate range for confidence intervals
	r := math.Max(h1-l1, h2-l2)

	// we know l1 < c1 < h1, c1 < c2, l2 < c2 < h2
	// therefore l1 < c1 < c2 < h2
	if h1 > l2 { // overlap
		overlapHigh, overlapLow := h1, l2
		if overlapHigh > h2 {
			overlapHigh = h2
		}
		if overlapLow < l1 {
			overlapLow = l1
		}
		return sign * (1 - (overlapHigh-overlapLow)/r) // perfect overlap == 0
	} else { // no overlap
		return sign * (1 + (l2-h1)/r) // just touching, l2 == h1, magnitude == 1, and then increases w/ the gap between intervals.
	}
}

func isHigherBetter(unit string) bool {
	return unit == "B/s" || strings.HasSuffix(unit, "ops/s") || strings.HasSuffix(unit, "ops/sec") || strings.HasSuffix(unit, "ops")
}

func worstRegression(b *BenchmarkJSON) *RegressionJSON {
	values := b.Values
	l := len(values)
	ninf := math.Inf(-1)

	sign := 1.0
	if b.HigherIsBetter {
		sign = -1.0
	}

	min := sign * values[l-1].Center
	worst := &RegressionJSON{
		DeltaIndex: -1,
		Change:     min,
		deltaScore: ninf,
	}

	if len(values) < 4 {
		worst.IgnoredBecause = "too few values"
		return worst
	}

	scores := []float64{}

	// First classify benchmarks that are too darn noisy, and get a feel for noisiness.
	for i := l - 1; i > 0; i-- {
		v1, v0 := values[i-1], values[i]
		score := math.Abs(changeScore(v1.Low, v1.Center, v1.High, v0.Low, v0.Center, v0.High))
		scores = append(scores, score)
	}

	sort.Float64s(scores)
	median := (scores[len(scores)/2] + scores[(len(scores)-1)/2]) / 2

	// Calculate threshold based on both median and the maximum score
	// This helps handle cases where there's a large change but the median is low
	maxScore := scores[len(scores)-1]

	// Adjust threshold calculation to be more sensitive to significant changes
	// Use a combination of median and max score, with a lower minimum threshold
	magicScoreThreshold := math.Max(0.8, math.Min(1.5*median, maxScore*0.4))

	// Log threshold calculation for debugging
	log.Printf("Regression threshold calculation for %s: median=%.3f, max=%.3f, threshold=%.3f",
		b.Name, median, maxScore, magicScoreThreshold)

	// Scan backwards looking for most recent outlier regression
	for i := l - 1; i > 0; i-- {
		v1, v0 := values[i-1], values[i]
		score := sign * changeScore(v1.Low, v1.Center, v1.High, v0.Low, v0.Center, v0.High)

		// Log each potential regression for debugging
		log.Printf("Checking regression at index %d: score=%.3f, threshold=%.3f, v1.Center=%.3f, v0.Center=%.3f",
			i, score, magicScoreThreshold, v1.Center, v0.Center)

		if score > magicScoreThreshold && sign*v1.Center < min && score > worst.deltaScore {
			worst.DeltaIndex = i
			worst.deltaScore = score
			worst.Delta = sign * (v0.Center - v1.Center)

			// Calculate the absolute change in percentage points
			diff := v0.Center - v1.Center

			// For ops/s and similar metrics where higher is better:
			// - If the value decreases (diff < 0), it's a regression
			// - If the value increases (diff > 0), it's an improvement
			if b.HigherIsBetter {
				worst.Change = -diff // Negative diff means regression
			} else {
				worst.Change = diff // Positive diff means regression
			}

			log.Printf("Calculating regression: v0=%.3f, v1=%.3f, diff=%.3f, higherIsBetter=%v, change=%.3f",
				v0.Center, v1.Center, diff, b.HigherIsBetter, worst.Change)

			// Log when we find a regression
			log.Printf("Found regression at index %d: score=%.3f, change=%.3f%%, delta=%.3f%%",
				i, score, worst.Change*100, worst.Delta*100)
		}

		min = math.Min(sign*v0.Center, min)
	}

	if worst.DeltaIndex == -1 {
		worst.IgnoredBecause = "didn't detect outlier regression"
		log.Printf("No regression detected for %s: max score=%.3f, threshold=%.3f",
			b.Name, maxScore, magicScoreThreshold)
	}

	return worst
}

type gzipResponseWriter struct {
	http.ResponseWriter
	w *gzip.Writer
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.w.Write(b)
}

const (
	defaultDays = 90
	maxDays     = 366
)

// search handles /dashboard/data.json.
//
// TODO(prattmic): Consider caching Influx results in-memory for a few mintures
// to reduce load on Influx.
func (a *App) dashboardData(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	days := uint64(defaultDays)
	dayParam := r.FormValue("days")
	if dayParam != "" {
		var err error
		days, err = strconv.ParseUint(dayParam, 10, 32)
		if err != nil {
			log.Printf("Error parsing days %q: %v", dayParam, err)
			http.Error(w, fmt.Sprintf("day parameter must be a positive integer less than or equal to %d", maxDays), http.StatusBadRequest)
			return
		}
		if days == 0 || days > maxDays {
			log.Printf("days %d too large", days)
			http.Error(w, fmt.Sprintf("day parameter must be a positive integer less than or equal to %d", maxDays), http.StatusBadRequest)
			return
		}
	}

	end := time.Now()
	endParam := r.FormValue("end")
	if endParam != "" {
		var err error
		end, err = time.Parse("2006-01-02", endParam)
		if err != nil {
			// For backward compatibility, try the old format as well
			end, err = time.Parse("2006-01-02T15:04", endParam)
			if err != nil {
				log.Printf("Error parsing end %q: %v", endParam, err)
				http.Error(w, "end parameter must be a date (YYYY-MM-DD)", http.StatusBadRequest)
				return
			}
		}
	}

	start := end.Add(-24 * time.Hour * time.Duration(days))

	// Get cloud and branch parameters first
	cloud := r.FormValue("cloud")
	if cloud == "" {
		cloud = "gce"
	}
	branch := r.FormValue("branch")
	if branch == "" {
		branch = "master"
	}

	// Get baseline configuration
	baselineCloud := r.FormValue("baseline_cloud")
	if baselineCloud == "" {
		baselineCloud = cloud // Default to same cloud as main selection
	}

	baselineBranch := r.FormValue("baseline_branch")
	if baselineBranch == "" {
		baselineBranch = branch // Default to same branch as main selection
	}

	// Parse baseline date
	var baselineStart, baselineEnd time.Time
	baselineDate := r.FormValue("baseline_date")

	baselineStart, err := time.Parse("2006-01-02", baselineDate)
	if err != nil {
		log.Printf("Error parsing baseline_date %q: %v", baselineDate, err)
		http.Error(w, "baseline_date parameter must be a date (YYYY-MM-DD)", http.StatusBadRequest)
		return
	}
	baselineEnd = baselineStart.Add(24 * time.Hour)
	log.Printf("Using baseline date: %s", baselineDate)

	log.Printf("Query time ranges: Current period: %s to %s (%d days); Baseline period: %s to %s",
		start.Format(time.RFC3339),
		end.Format(time.RFC3339),
		days,
		baselineStart.Format(time.RFC3339),
		baselineEnd.Format(time.RFC3339))

	methStart := time.Now()
	defer func() {
		log.Printf("Dashboard total query time: %s", time.Since(methStart))
	}()

	vmClient := NewVictoriaMetricsClient(a.VictoriaMetricsURL)

	benchmark := r.FormValue("benchmark")
	unit := r.FormValue("unit")

	// First, try to get the list of available metrics
	metricsURL := fmt.Sprintf("%s/api/v1/series", a.VictoriaMetricsURL)
	metricsReq, err := http.NewRequestWithContext(ctx, "GET", metricsURL, nil)
	if err != nil {
		log.Printf("Error creating request for metrics: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Add query parameters to find any metrics
	if benchmark != "" {
		// If we have a benchmark name, query for that specific test
		q := metricsReq.URL.Query()
		q.Add("match[]", fmt.Sprintf(`{test="%s", unit!=""}`, benchmark))
		q.Add("start", fmt.Sprintf("%d", start.Unix()))
		q.Add("end", fmt.Sprintf("%d", end.Unix()))
		metricsReq.URL.RawQuery = q.Encode()

		// Make the request to find available metrics
		client := &http.Client{}
		metricsResp, err := client.Do(metricsReq)
		if err != nil {
			log.Printf("Error querying VictoriaMetrics for metrics: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		defer metricsResp.Body.Close()

		metricsBody, err := io.ReadAll(metricsResp.Body)
		if err != nil {
			log.Printf("Error reading response: %v", err)
			http.Error(w, "Error reading response", http.StatusInternalServerError)
			return
		}

		var seriesResponse struct {
			Status string              `json:"status"`
			Data   []map[string]string `json:"data"`
		}
		if err := json.Unmarshal(metricsBody, &seriesResponse); err == nil &&
			seriesResponse.Status == "success" && len(seriesResponse.Data) > 0 {

			// Extract unique metric names
			metricNames := make([]string, 0)
			for _, series := range seriesResponse.Data {
				if metricName, ok := series["__name__"]; ok {
					found := false
					for _, existing := range metricNames {
						if existing == metricName {
							found = true
							break
						}
					}
					if !found {
						metricNames = append(metricNames, metricName)
					}
				}
			}

			if len(metricNames) > 0 {
				log.Printf("Found metrics for test %s: %v", benchmark, metricNames)

				// Process each metric individually
				var allBenchmarks []*BenchmarkJSON
				var allCommits []Commit
				commitsMap := make(map[string]Commit) // To track unique commits

				for _, metricName := range metricNames {
					var metricQuery string
					if unit != "" {
						// Specific benchmark and unit query
						log.Printf("Querying for metric=%s, test=%s, cloud=%s, branch=%s, unit=%s",
							metricName, benchmark, cloud, branch, unit)
						metricQuery = fmt.Sprintf(`avg_over_time(%s{test="%s",cloud="%s",branch="%s",unit="%s"}[1d])`,
							metricName, benchmark, cloud, branch, unit)
					} else {
						// Query for all units of this test
						var benchmarkFilter string
						if benchmark != "" {
							benchmarkFilter = fmt.Sprintf(`,test=~"%s"`, benchmark)
						} else {
							benchmarkFilter = ""
						}

						log.Printf("Querying for metric=%s, cloud=%s, branch=%s, test filter=%s",
							metricName, cloud, branch, benchmarkFilter)
						metricQuery = fmt.Sprintf(`avg_over_time(%s{cloud="%s",branch="%s"%s,unit!=""}[1d])`,
							metricName, cloud, branch, benchmarkFilter)
					}

					// Get data for this metric
					metricData, err := vmClient.Query(ctx, metricQuery, start, end)
					if err != nil {
						log.Printf("Error querying metric %s: %v", metricName, err)
						continue
					}

					// Get baseline data for this metric if needed
					var metricBaselineData []byte
					if baselineDate != "" {
						// Use baseline configuration for the query
						baselineQuery := strings.Replace(metricQuery,
							fmt.Sprintf(`cloud="%s"`, cloud),
							fmt.Sprintf(`cloud="%s"`, baselineCloud), 1)
						baselineQuery = strings.Replace(baselineQuery,
							fmt.Sprintf(`branch="%s"`, branch),
							fmt.Sprintf(`branch="%s"`, baselineBranch), 1)

						metricBaselineData, err = vmClient.Query(ctx, baselineQuery, baselineStart, baselineEnd)
						if err != nil {
							log.Printf("Error querying baseline for metric %s: %v", metricName, err)
							// Continue without baseline data
						}
					} else {
						// Use the old baseline days approach
						metricBaselineData, err = vmClient.Query(ctx, metricQuery, baselineStart, baselineEnd)
						if err != nil {
							log.Printf("Error querying baseline for metric %s: %v", metricName, err)
							// Continue without baseline data
						}
					}

					// Parse the response for this metric
					metricBenchmarks, err := parseVictoriaMetricsResponse(metricData, true, metricBaselineData)
					if err != nil {
						log.Printf("Error parsing response for metric %s: %v", metricName, err)
						continue
					}

					if len(metricBenchmarks) > 0 {
						log.Printf("Found %d benchmarks for metric %s", len(metricBenchmarks), metricName)

						// Add benchmarks to the combined results
						allBenchmarks = append(allBenchmarks, metricBenchmarks...)

						// Add commits to the combined results
						metricCommits := commitsFromBenchmarks(metricBenchmarks)
						for _, commit := range metricCommits {
							commitsMap[commit.Hash] = commit
						}
					} else {
						log.Printf("No benchmarks found for metric %s", metricName)
					}
				}

				// Convert commits map to slice
				for _, commit := range commitsMap {
					allCommits = append(allCommits, commit)
				}

				// Sort commits by date
				sort.Slice(allCommits, func(i, j int) bool {
					return allCommits[i].Date.Before(allCommits[j].Date)
				})

				if len(allBenchmarks) > 0 {
					// Write the combined response
					w.Header().Set("Content-Type", "application/json")

					if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
						w.Header().Set("Content-Encoding", "gzip")
						gz := gzip.NewWriter(w)
						defer gz.Close()
						w = &gzipResponseWriter{w: gz, ResponseWriter: w}
					}

					if err := json.NewEncoder(w).Encode(&DataJSON{Benchmarks: allBenchmarks, Commits: allCommits}); err != nil {
						log.Printf("Error encoding results: %v", err)
						http.Error(w, "Internal error, see logs", 500)
					}
					return
				}

				// If we get here, it means we either didn't find any metrics or none of the metrics had data
				log.Printf("No benchmarks found for test: %s", benchmark)
				http.Error(w, "No benchmarks found", 404)
				return
			}
		}
	}

	// If we get here, it means we either didn't find any metrics or none of the metrics had data
	log.Printf("No benchmarks found for test: %s", benchmark)
	http.Error(w, "No benchmarks found", 404)
}

// parseVictoriaMetricsResponse parses the VictoriaMetrics response into BenchmarkJSON format
func parseVictoriaMetricsResponse(data []byte, hasBaseline bool, baselineData []byte) ([]*BenchmarkJSON, error) {
	// First try to use the benchfmt-based comparison if both current and baseline data are available
	if hasBaseline && baselineData != nil {
		// Try the new comparison method using benchfmt
		log.Printf("Attempting to parse data with benchfmt comparison method")
		currentMetrics, err := convertVMDataToMetricPoints(data)
		if err == nil && len(currentMetrics) > 0 {
			baselineMetrics, err := convertVMDataToMetricPoints(baselineData)
			if err == nil && len(baselineMetrics) > 0 {
				// Log metrics counts for debugging
				log.Printf("Found %d current metrics and %d baseline metrics for benchfmt comparison",
					len(currentMetrics), len(baselineMetrics))

				// Log all current metric keys and their point counts
				log.Printf("Current metrics:")
				for key, points := range currentMetrics {
					log.Printf("  - %s: %d points", key, len(points))
					if len(points) > 0 {
						startTime := points[0].Timestamp.Format(time.RFC3339)
						endTime := points[len(points)-1].Timestamp.Format(time.RFC3339)
						log.Printf("    Time range: %s to %s", startTime, endTime)
					}
				}

				// Log all baseline metric keys and their point counts
				log.Printf("Baseline metrics:")
				for key, points := range baselineMetrics {
					log.Printf("  - %s: %d points", key, len(points))
					if len(points) > 0 {
						startTime := points[0].Timestamp.Format(time.RFC3339)
						endTime := points[len(points)-1].Timestamp.Format(time.RFC3339)
						log.Printf("    Time range: %s to %s", startTime, endTime)
					}
				}

				// Try to create comparisons using the benchfmt approach
				compBenchmarks, err := createBenchmarkComparisons(currentMetrics, baselineMetrics)
				if err == nil && len(compBenchmarks) > 0 {
					log.Printf("Successfully created %d benchmark comparisons using benchfmt", len(compBenchmarks))

					// Log the comparisons created
					for i, b := range compBenchmarks {
						log.Printf("Comparison %d: %s (%s) with %d values",
							i, b.Name, b.Unit, len(b.Values))
					}

					// Calculate regressions for each benchmark
					for _, benchmark := range compBenchmarks {
						benchmark.Regression = worstRegression(benchmark)
					}

					return compBenchmarks, nil
				} else if err != nil {
					log.Printf("Error creating benchmark comparisons: %v", err)
				} else {
					log.Printf("No comparisons created with benchfmt method - no matching metrics between current and baseline")
				}
			} else {
				if err != nil {
					log.Printf("Error converting baseline data to metric points: %v", err)
				} else {
					log.Printf("No baseline metrics found in VictoriaMetrics response")

					// Check for baseline data but empty results
					if baselineData != nil {
						var baselinePreview string
						if len(baselineData) > 500 {
							baselinePreview = string(baselineData[:500]) + "... [truncated]"
						} else {
							baselinePreview = string(baselineData)
						}
						log.Printf("Baseline data exists but no metrics extracted. Baseline data preview: %s", baselinePreview)
					}
				}
			}
		} else {
			if err != nil {
				log.Printf("Error converting current data to metric points: %v", err)
			} else {
				log.Printf("No current metrics found in VictoriaMetrics response")

				// Check for current data but empty results
				if data != nil {
					var dataPreview string
					if len(data) > 500 {
						dataPreview = string(data[:500]) + "... [truncated]"
					} else {
						dataPreview = string(data)
					}
					log.Printf("Current data exists but no metrics extracted. Current data preview: %s", dataPreview)
				}
			}
		}
	}

	// Fall back to the original implementation
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric struct {
					Name           string `json:"__name__"`
					Test           string `json:"test"`
					Unit           string `json:"unit"`
					Branch         string `json:"branch"`
					Cloud          string `json:"cloud"`
					Goos           string `json:"goos"`
					Goarch         string `json:"goarch"`
					TeamCityRunID  string `json:"test_run_id"`
					Commit         string `json:"commit"`
					IsHigherBetter string `json:"is_higher_better"`
				} `json:"metric"`
				Values [][]interface{} `json:"values"` // [timestamp, value] pairs
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.Unmarshal(data, &response); err != nil {
		log.Printf("Error unmarshaling response: %v", err)
		log.Printf("Raw response: %s", string(data))
		return nil, fmt.Errorf("error unmarshaling response: %w", err)
	}

	if response.Status != "success" {
		return nil, fmt.Errorf("unexpected status: %s", response.Status)
	}

	// Log the response data for debugging
	log.Printf("Response status: %s, result type: %s, result count: %d",
		response.Status, response.Data.ResultType, len(response.Data.Result))

	// First check if we have baseline data we can use for commit hashes
	baselineCommitsByDate := make(map[string]string)

	if hasBaseline && baselineData != nil {
		// Parse baseline response to extract baseline commits
		var baselineResponse struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Metric struct {
						Name           string `json:"__name__"`
						Test           string `json:"test"`
						Unit           string `json:"unit"`
						Branch         string `json:"branch"`
						Cloud          string `json:"cloud"`
						TeamCityRunID  string `json:"test_run_id"`
						Commit         string `json:"commit"`
						IsHigherBetter string `json:"is_higher_better"`
					} `json:"metric"`
					Values [][]interface{} `json:"values"`
				} `json:"result"`
			} `json:"data"`
		}

		if err := json.Unmarshal(baselineData, &baselineResponse); err == nil &&
			baselineResponse.Status == "success" {

			// Collect all baseline commits indexed by date
			for _, result := range baselineResponse.Data.Result {
				for _, v := range result.Values {
					if len(v) != 2 {
						continue
					}

					ts, ok := v[0].(float64)
					if !ok {
						continue
					}

					// Get commit hash from baseline
					baselineCommit := ""
					if result.Metric.Commit != "" {
						baselineCommit = result.Metric.Commit
					} else if result.Metric.TeamCityRunID != "" {
						baselineCommit = extractTeamCityRunID(result.Metric.TeamCityRunID)
					} else {
						baselineCommit = "baseline-" + time.Unix(int64(ts), 0).Format("20060102")
					}

					// Store using date as key
					dateKey := time.Unix(int64(ts), 0).Format("2006-01-02")
					baselineCommitsByDate[dateKey] = baselineCommit
				}
			}

			log.Printf("Collected %d baseline commits by date for regular data flow", len(baselineCommitsByDate))
		} else if err != nil {
			log.Printf("Error parsing baseline data for commit lookup: %v", err)
		}
	}

	if len(response.Data.Result) == 0 {
		log.Printf("No metrics found in VictoriaMetrics response")

		// Try to extract more details about the empty response
		var rawResponse map[string]interface{}
		if err := json.Unmarshal(data, &rawResponse); err == nil {
			if data, ok := rawResponse["data"].(map[string]interface{}); ok {
				log.Printf("Data object keys: %v", maps.Keys(data))
				if resultType, ok := data["resultType"].(string); ok {
					log.Printf("Result type: %s", resultType)
				}
				// Look for any extra fields that might provide context
				for k, v := range data {
					if k != "result" && k != "resultType" {
						log.Printf("Additional data field: %s = %v", k, v)
					}
				}
			}
		}

		if hasBaseline {
			log.Printf("Attempting to use baseline data anyway")

			// Try with baseline data
			var baselineResponse struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string `json:"resultType"`
					Result     []struct {
						Metric struct {
							Name           string `json:"__name__"`
							Test           string `json:"test"`
							Unit           string `json:"unit"`
							Branch         string `json:"branch"`
							Cloud          string `json:"cloud"`
							TeamCityRunID  string `json:"test_run_id"`
							Commit         string `json:"commit"`
							IsHigherBetter string `json:"is_higher_better"`
						} `json:"metric"`
						Values [][]interface{} `json:"values"`
					} `json:"result"`
				} `json:"data"`
			}

			if err := json.Unmarshal(baselineData, &baselineResponse); err == nil &&
				baselineResponse.Status == "success" && len(baselineResponse.Data.Result) > 0 {

				log.Printf("Found %d metrics in baseline data, using those",
					len(baselineResponse.Data.Result))

				// Create benchmarks from baseline data
				benchmarks := make([]*BenchmarkJSON, 0)

				// Create a map of baseline commits by date for later lookup
				baselineCommitsByDate := make(map[string]string)

				// First pass: collect all baseline commits
				for _, result := range baselineResponse.Data.Result {
					for _, v := range result.Values {
						if len(v) != 2 {
							continue
						}

						ts, ok := v[0].(float64)
						if !ok {
							continue
						}

						// Get commit hash from baseline
						baselineCommit := ""
						if result.Metric.Commit != "" {
							baselineCommit = result.Metric.Commit
						} else if result.Metric.TeamCityRunID != "" {
							baselineCommit = extractTeamCityRunID(result.Metric.TeamCityRunID)
						} else {
							baselineCommit = "baseline-" + time.Unix(int64(ts), 0).Format("20060102")
						}

						// Store using date as key
						dateKey := time.Unix(int64(ts), 0).Format("2006-01-02")
						baselineCommitsByDate[dateKey] = baselineCommit
					}
				}

				log.Printf("Collected baseline commits for %d dates", len(baselineCommitsByDate))

				// Second pass: create benchmarks with actual baseline commit hashes
				for _, result := range baselineResponse.Data.Result {
					if result.Metric.Test == "" || result.Metric.Unit == "" {
						continue
					}

					benchmark := &BenchmarkJSON{
						Name:           result.Metric.Test,
						Unit:           result.Metric.Unit,
						HigherIsBetter: getHigherBetter(result.Metric.IsHigherBetter),
						Values:         []ValueJSON{},
					}

					// Add values from baseline
					for _, v := range result.Values {
						if len(v) != 2 {
							continue
						}

						ts, ok := v[0].(float64)
						if !ok {
							continue
						}

						var valStr string
						switch val := v[1].(type) {
						case string:
							valStr = val
						case float64:
							valStr = fmt.Sprintf("%f", val)
						default:
							continue
						}

						value, err := strconv.ParseFloat(valStr, 64)
						if err != nil {
							continue
						}

						// Use commit field if available, then fallback to TeamCityRunID, then timestamp
						commitHash := fmt.Sprintf("%d", int64(ts)) // Fallback to timestamp
						if result.Metric.Commit != "" {
							commitHash = result.Metric.Commit
						} else if result.Metric.TeamCityRunID != "" {
							commitHash = extractTeamCityRunID(result.Metric.TeamCityRunID)
						}

						// Look up baseline commit by date
						dateKey := time.Unix(int64(ts), 0).Format("2006-01-02")
						baselineCommitHash := baselineCommitsByDate[dateKey]
						if baselineCommitHash == "" {
							// If no baseline commit found, use a placeholder
							baselineCommitHash = "baseline-" + dateKey
						}

						benchmark.Values = append(benchmark.Values, ValueJSON{
							CommitDate:           time.Unix(int64(ts), 0),
							CommitHash:           commitHash,
							BaselineCommitHash:   baselineCommitHash, // Use the actual baseline commit hash
							BaselineCommitDate:   time.Unix(int64(ts), 0),
							BenchmarksCommitHash: "benchmarks",

							Low:    value, // Estimate confidence interval
							Center: value,
							High:   value, // Estimate confidence interval
						})
					}

					if len(benchmark.Values) > 0 {
						sort.Slice(benchmark.Values, func(i, j int) bool {
							return benchmark.Values[i].CommitDate.Before(benchmark.Values[j].CommitDate)
						})
						benchmarks = append(benchmarks, benchmark)
					}
				}

				if len(benchmarks) > 0 {
					log.Printf("Created %d benchmarks from baseline data", len(benchmarks))
					return benchmarks, nil
				}
			} else {
				log.Printf("No usable metrics found in baseline data either")
				if err != nil {
					log.Printf("Error parsing baseline data: %v", err)
				}
			}
		}

		// Modified the error message to be more specific about the empty result
		return nil, fmt.Errorf("no metrics found in response (result array is empty)")
	}

	// Group results by benchmark name and unit
	benchmarks := make(map[string]*BenchmarkJSON)

	for _, result := range response.Data.Result {
		// Log the metric details for debugging
		log.Printf("Processing metric: %v", result.Metric)

		// Extract unit
		unit := "ops/s"           // default
		unit = result.Metric.Unit // Access unit as a struct field

		// Create the key for this metric series
		testName := result.Metric.Test // Access test as a struct field
		if testName == "" {
			log.Printf("Skipping metric with empty test name: %v", result.Metric)
			continue
		}

		// Try to get the metric name - use the Name field directly
		metricName := result.Metric.Name

		metricKey := fmt.Sprintf("%s_%s", testName, unit)
		log.Printf("Processing metric key: %s (metric name: %s) with %d values",
			metricKey, metricName, len(result.Values))

		benchmark, ok := benchmarks[metricKey]
		if !ok {
			benchmark = &BenchmarkJSON{
				Name:           testName,
				Unit:           unit,
				HigherIsBetter: isHigherBetter(unit),
				Values:         []ValueJSON{},
			}
			benchmarks[metricKey] = benchmark
		}

		// Convert values to ValueJSON format
		for _, v := range result.Values {
			if len(v) != 2 {
				log.Printf("Skipping invalid data point in metric %s: %v", metricKey, v)
				continue // Skip invalid data points
			}

			// Extract timestamp and value from the array
			ts, ok := v[0].(float64)
			if !ok {
				log.Printf("Invalid timestamp format in metric %s: %v", metricKey, v[0])
				continue
			}

			// Value might be either string or float64 depending on the VictoriaMetrics version
			var valStr string
			switch val := v[1].(type) {
			case string:
				valStr = val
			case float64:
				valStr = fmt.Sprintf("%f", val)
			default:
				log.Printf("Invalid value format in metric %s: %v", metricKey, v[1])
				continue
			}

			value, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				log.Printf("Error parsing value in metric %s: %v", metricKey, err)
				continue
			}

			// Use test_run_id as commit hash if available
			commitHash := fmt.Sprintf("%d", int64(ts)) // Fallback to timestamp
			if result.Metric.TeamCityRunID != "" {
				commitHash = extractTeamCityRunID(result.Metric.TeamCityRunID)
			}

			// Create a ValueJSON with confidence intervals
			benchmark.Values = append(benchmark.Values, ValueJSON{
				CommitDate:           time.Unix(int64(ts), 0),
				CommitHash:           commitHash,
				BaselineCommitHash:   commitHash, // Use the same commit hash as baseline
				BaselineCommitDate:   time.Unix(int64(ts), 0),
				BenchmarksCommitHash: "benchmarks",
				Low:                  value, // Estimate confidence interval
				Center:               value,
				High:                 value, // Estimate confidence interval
			})
		}
	}

	// Convert map to slice
	result := make([]*BenchmarkJSON, 0, len(benchmarks))
	for _, benchmark := range benchmarks {
		// Sort values by commit date
		sort.Slice(benchmark.Values, func(i, j int) bool {
			return benchmark.Values[i].CommitDate.Before(benchmark.Values[j].CommitDate)
		})
		result = append(result, benchmark)
	}

	return result, nil
}

// logBaselineStats logs statistics about baseline comparison matching to help with debugging
func logBaselineStats(benchmarks []*BenchmarkJSON, baselineValues map[string]map[string]float64) {
	matchCount := 0
	totalPoints := 0

	for _, benchmark := range benchmarks {
		key := fmt.Sprintf("%s_%s", benchmark.Name, benchmark.Unit)
		if baselineMap, ok := baselineValues[key]; ok {
			// Count baseline matches for this benchmark
			benchMatchCount := 0
			for _, v := range benchmark.Values {
				dateStr := v.CommitDate.Format("2006-01-02")
				if _, hasMatch := baselineMap[dateStr]; hasMatch {
					benchMatchCount++
				}
			}

			matchRate := 0.0
			if len(benchmark.Values) > 0 {
				matchRate = float64(benchMatchCount) / float64(len(benchmark.Values)) * 100
			}

			log.Printf("Baseline stats for %s: %d/%d points matched (%.1f%%)",
				key, benchMatchCount, len(benchmark.Values), matchRate)

			matchCount += benchMatchCount
			totalPoints += len(benchmark.Values)
		} else {
			log.Printf("No baseline data found for %s", key)
			totalPoints += len(benchmark.Values)
		}
	}

	// Log overall statistics
	overallRate := 0.0
	if totalPoints > 0 {
		overallRate = float64(matchCount) / float64(totalPoints) * 100
	}
	log.Printf("Overall baseline match rate: %d/%d points (%.1f%%)",
		matchCount, totalPoints, overallRate)

	if matchCount == 0 && totalPoints > 0 {
		log.Printf("WARNING: No baseline matches found! Check that the baseline period contains data.")
	}
}

func commitsFromBenchmarks(benchmarks []*BenchmarkJSON) []Commit {
	commitsMap := make(map[string]Commit)
	for _, b := range benchmarks {
		for _, v := range b.Values {
			commitsMap[v.CommitHash] = Commit{Hash: v.CommitHash, Date: v.CommitDate}
		}
	}
	commits := maps.Values(commitsMap)
	sort.Slice(commits, func(i, j int) bool {
		return commits[i].Date.Before(commits[j].Date)
	})
	return commits
}

type Commit struct {
	Hash string
	Date time.Time
}

// formFields handles the formfields.json endpoint.
func (a *App) formFields(w http.ResponseWriter, r *http.Request) {
	// Form the response.
	resp := FormFieldsJSON{
		Branches:            []string{"master"},
		LatestReleaseBranch: "",
		GrafanaURL:          a.GrafanaURL,
	}

	// Encode and write the response.
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(&resp); err != nil {
		log.Printf("Error encoding results: %v", err)
		http.Error(w, "Internal error, see logs", 500)
	}
}

type FormFieldsJSON struct {
	Branches            []string
	LatestReleaseBranch string
	GrafanaURL          string
}

// dashboardTests handles the tests.json endpoint
func (a *App) dashboardTests(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Calculate the start time (1 year ago)
	start := time.Now().Add(-365 * 24 * time.Hour)

	// Construct the VictoriaMetrics query URL
	url := fmt.Sprintf("%s/api/v1/label/test/values", a.VictoriaMetricsURL)

	// Create the request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Add query parameters
	q := req.URL.Query()
	q.Add("start", fmt.Sprintf("%d", start.Unix()))
	req.URL.RawQuery = q.Encode()

	// Make the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error querying VictoriaMetrics: %v", err)
		http.Error(w, "Error querying VictoriaMetrics", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Read the response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		http.Error(w, "Error reading response", http.StatusInternalServerError)
		return
	}

	// Parse the response
	var vmResponse struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &vmResponse); err != nil {
		log.Printf("Error parsing response: %v", err)
		http.Error(w, "Error parsing response", http.StatusInternalServerError)
		return
	}

	if vmResponse.Status != "success" {
		log.Printf("Unexpected status from VictoriaMetrics: %s", vmResponse.Status)
		http.Error(w, "Error from VictoriaMetrics", http.StatusInternalServerError)
		return
	}

	// Return the tests
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(TestsJSON{Tests: vmResponse.Data}); err != nil {
		log.Printf("Error encoding response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// MetricsJSON is the response for the metrics.json endpoint
type MetricsJSON struct {
	Metrics []string `json:"metrics"`
}

// listMetrics handles the metrics.json endpoint, returning a list of all available metrics
func (a *App) listMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Calculate the start time (30 days ago)
	start := time.Now().Add(-30 * 24 * time.Hour)

	// Construct the VictoriaMetrics query URL
	url := fmt.Sprintf("%s/api/v1/label/__name__/values", a.VictoriaMetricsURL)

	// Create the request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Add query parameters
	q := req.URL.Query()
	q.Add("start", fmt.Sprintf("%d", start.Unix()))
	req.URL.RawQuery = q.Encode()

	// Make the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error querying VictoriaMetrics: %v", err)
		http.Error(w, "Error querying VictoriaMetrics", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	// Read the response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		http.Error(w, "Error reading response", http.StatusInternalServerError)
		return
	}

	// Parse the response
	var vmResponse struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &vmResponse); err != nil {
		log.Printf("Error parsing response: %v", err)
		http.Error(w, "Error parsing response", http.StatusInternalServerError)
		return
	}

	if vmResponse.Status != "success" {
		log.Printf("Unexpected status from VictoriaMetrics: %s", vmResponse.Status)
		http.Error(w, "Error from VictoriaMetrics", http.StatusInternalServerError)
		return
	}

	// Return the metrics
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(MetricsJSON{Metrics: vmResponse.Data}); err != nil {
		log.Printf("Error encoding response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// TestInfoJSON is the response for the test_info.json endpoint
type TestInfoJSON struct {
	Test         string              `json:"test"`
	MetricsCount int                 `json:"metrics_count"`
	Labels       map[string][]string `json:"labels"`
	Metrics      []string            `json:"metrics"`
	SampleData   []map[string]any    `json:"sample_data"`
}

// testInfo handles the test_info.json endpoint, returning detailed information about a specific test
func (a *App) testInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get the test name from the query parameters
	testName := r.FormValue("test")
	if testName == "" {
		http.Error(w, "test parameter is required", http.StatusBadRequest)
		return
	}

	// Calculate the start time (30 days ago)
	start := time.Now().Add(-30 * 24 * time.Hour)
	end := time.Now()

	// 1. First get all metrics associated with this test
	metricsURL := fmt.Sprintf("%s/api/v1/series", a.VictoriaMetricsURL)
	metricsReq, err := http.NewRequestWithContext(ctx, "GET", metricsURL, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Add query parameters
	q := metricsReq.URL.Query()
	q.Add("match[]", fmt.Sprintf(`{test="%s"}`, testName))
	q.Add("start", fmt.Sprintf("%d", start.Unix()))
	q.Add("end", fmt.Sprintf("%d", end.Unix()))
	metricsReq.URL.RawQuery = q.Encode()

	// Make the request
	client := &http.Client{}
	metricsResp, err := client.Do(metricsReq)
	if err != nil {
		log.Printf("Error querying VictoriaMetrics: %v", err)
		http.Error(w, "Error querying VictoriaMetrics", http.StatusInternalServerError)
		return
	}
	defer metricsResp.Body.Close()

	// Read the response
	metricsBody, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		http.Error(w, "Error reading response", http.StatusInternalServerError)
		return
	}

	// Parse the response
	var seriesResponse struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(metricsBody, &seriesResponse); err != nil {
		log.Printf("Error parsing response: %v", err)
		log.Printf("Raw response: %s", string(metricsBody))
		http.Error(w, "Error parsing response", http.StatusInternalServerError)
		return
	}

	if seriesResponse.Status != "success" {
		log.Printf("Unexpected status from VictoriaMetrics: %s", seriesResponse.Status)
		http.Error(w, "Error from VictoriaMetrics", http.StatusInternalServerError)
		return
	}

	if len(seriesResponse.Data) == 0 {
		log.Printf("No metrics found for test: %s", testName)
		http.Error(w, fmt.Sprintf("No metrics found for test: %s", testName), http.StatusNotFound)
		return
	}

	// Process the response to extract metrics and labels
	metrics := make([]string, 0)
	labels := make(map[string][]string)
	sampleData := make([]map[string]any, 0, 5) // Sample of up to 5 metrics

	// Extract all unique metrics and labels
	for i, series := range seriesResponse.Data {
		if metricName, ok := series["__name__"]; ok {
			metrics = append(metrics, metricName)
		}

		// For the first few series, save them as sample data
		if i < 5 {
			// Convert map[string]string to map[string]any
			anySeries := make(map[string]any)
			for k, v := range series {
				anySeries[k] = v
			}
			sampleData = append(sampleData, anySeries)
		}

		// Collect all labels
		for labelName, labelValue := range series {
			if labelName == "__name__" {
				continue // Skip the metric name itself
			}

			// Add to the list of values for this label
			found := false
			for _, existing := range labels[labelName] {
				if existing == labelValue {
					found = true
					break
				}
			}
			if !found {
				labels[labelName] = append(labels[labelName], labelValue)
			}
		}
	}

	// Return the test information
	response := TestInfoJSON{
		Test:         testName,
		MetricsCount: len(seriesResponse.Data),
		Labels:       labels,
		Metrics:      metrics,
		SampleData:   sampleData,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Error encoding response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

// SeriesDataJSON is the response for the series_data.json endpoint
type SeriesDataJSON struct {
	Benchmarks []*BenchmarkJSON `json:"benchmarks"`
	Commits    []Commit         `json:"commits"`
}

// seriesDataToBenchmark handles converting VictoriaMetrics series data to BenchmarkJSON format
func (a *App) seriesDataToBenchmark(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get the test name from the query parameters
	testName := r.FormValue("test")
	if testName == "" {
		http.Error(w, "test parameter is required", http.StatusBadRequest)
		return
	}

	cloud := r.FormValue("cloud")
	if cloud == "" {
		cloud = "gce"
	}
	branch := r.FormValue("branch")
	if branch == "" {
		branch = "master"
	}

	// Calculate time range
	days := uint64(defaultDays)
	dayParam := r.FormValue("days")
	if dayParam != "" {
		var err error
		days, err = strconv.ParseUint(dayParam, 10, 32)
		if err != nil {
			log.Printf("Error parsing days %q: %v", dayParam, err)
			http.Error(w, fmt.Sprintf("day parameter must be a positive integer less than or equal to %d", maxDays), http.StatusBadRequest)
			return
		}
		if days == 0 || days > maxDays {
			log.Printf("days %d too large", days)
			http.Error(w, fmt.Sprintf("day parameter must be a positive integer less than or equal to %d", maxDays), http.StatusBadRequest)
			return
		}
	}

	end := time.Now()
	endParam := r.FormValue("end")
	if endParam != "" {
		var err error
		end, err = time.Parse("2006-01-02", endParam)
		if err != nil {
			// For backward compatibility, try the old format as well
			end, err = time.Parse("2006-01-02T15:04", endParam)
			if err != nil {
				log.Printf("Error parsing end %q: %v", endParam, err)
				http.Error(w, "end parameter must be a date (YYYY-MM-DD)", http.StatusBadRequest)
				return
			}
		}
	}

	start := end.Add(-24 * time.Hour * time.Duration(days))

	// 1. First get all metrics associated with this test
	metricsURL := fmt.Sprintf("%s/api/v1/series", a.VictoriaMetricsURL)
	metricsReq, err := http.NewRequestWithContext(ctx, "GET", metricsURL, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Add query parameters
	q := metricsReq.URL.Query()
	q.Add("match[]", fmt.Sprintf(`{test="%s",cloud="%s",branch="%s"}`, testName, cloud, branch))
	q.Add("start", fmt.Sprintf("%d", start.Unix()))
	q.Add("end", fmt.Sprintf("%d", end.Unix()))
	metricsReq.URL.RawQuery = q.Encode()

	// Make the request
	client := &http.Client{}
	metricsResp, err := client.Do(metricsReq)
	if err != nil {
		log.Printf("Error querying VictoriaMetrics: %v", err)
		http.Error(w, "Error querying VictoriaMetrics", http.StatusInternalServerError)
		return
	}
	defer metricsResp.Body.Close()

	// Read the response
	metricsBody, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		log.Printf("Error reading response: %v", err)
		http.Error(w, "Error reading response", http.StatusInternalServerError)
		return
	}

	// Parse the response
	var seriesResponse struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(metricsBody, &seriesResponse); err != nil {
		log.Printf("Error parsing response: %v", err)
		log.Printf("Raw response: %s", string(metricsBody))
		http.Error(w, "Error parsing response", http.StatusInternalServerError)
		return
	}

	if seriesResponse.Status != "success" {
		log.Printf("Unexpected status from VictoriaMetrics: %s", seriesResponse.Status)
		http.Error(w, "Error from VictoriaMetrics", http.StatusInternalServerError)
		return
	}

	if len(seriesResponse.Data) == 0 {
		log.Printf("No metrics found for test: %s", testName)
		http.Error(w, fmt.Sprintf("No metrics found for test: %s", testName), http.StatusNotFound)
		return
	}

	// Extract the metric names
	metricNames := make(map[string]bool)
	for _, series := range seriesResponse.Data {
		if metricName, ok := series["__name__"]; ok {
			metricNames[metricName] = true
		}
	}

	// Collect all time series data for each metric
	benchmarkData := make([]*BenchmarkJSON, 0)
	for metricName := range metricNames {
		// Query data for this specific metric
		query := fmt.Sprintf(`%s{test="%s",cloud="%s",branch="%s"}`, metricName, testName, cloud, branch)
		log.Printf("Querying for metric %s data", metricName)

		// Fetch data for this metric
		data, err := a.vmClient.Query(ctx, query, start, end)
		if err != nil {
			log.Printf("Error querying VictoriaMetrics for metric %s: %v", metricName, err)
			continue
		}

		// Parse the response for this metric
		var metricResponse struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string `json:"resultType"`
				Result     []struct {
					Metric struct {
						Name           string `json:"__name__"`
						Test           string `json:"test"`
						Unit           string `json:"unit"`
						Branch         string `json:"branch"`
						Cloud          string `json:"cloud"`
						Goos           string `json:"goos"`
						Goarch         string `json:"goarch"`
						TeamCityRunID  string `json:"test_run_id"`
						Commit         string `json:"commit"`
						IsHigherBetter string `json:"is_higher_better"`
					} `json:"metric"`
					Values [][]interface{} `json:"values"` // [timestamp, value] pairs
				} `json:"result"`
			} `json:"data"`
		}

		if err := json.Unmarshal(data, &metricResponse); err != nil {
			log.Printf("Error unmarshaling metric %s response: %v", metricName, err)
			continue
		}

		if metricResponse.Status != "success" {
			log.Printf("Unexpected status from VictoriaMetrics for metric %s: %s", metricName, metricResponse.Status)
			continue
		}

		// Convert each result to BenchmarkJSON
		for _, result := range metricResponse.Data.Result {
			// Create a benchmark object for this result
			unit := "ops/s" // default unit
			if result.Metric.Unit != "" {
				unit = result.Metric.Unit
			}

			// Create a unique name based on the metric and labels
			name := result.Metric.Name
			if result.Metric.Test != "" {
				name = result.Metric.Test
			}

			// Create a BenchmarkJSON object
			benchmark := &BenchmarkJSON{
				Name:           name,
				Unit:           unit,
				HigherIsBetter: isHigherBetter(unit),
				Values:         make([]ValueJSON, 0, len(result.Values)),
			}

			// Convert each value to ValueJSON
			for _, v := range result.Values {
				if len(v) != 2 {
					continue // Skip invalid data points
				}

				// Extract timestamp and value
				ts, ok := v[0].(float64)
				if !ok {
					continue
				}

				// Parse the value
				var valStr string
				switch val := v[1].(type) {
				case string:
					valStr = val
				case float64:
					valStr = fmt.Sprintf("%f", val)
				default:
					continue
				}

				value, err := strconv.ParseFloat(valStr, 64)
				if err != nil {
					continue
				}

				// Use commit field if available, then fallback to TeamCityRunID, then timestamp
				commitHash := fmt.Sprintf("%d", int64(ts)) // Fallback to timestamp
				if result.Metric.Commit != "" {
					commitHash = result.Metric.Commit
				} else if result.Metric.TeamCityRunID != "" {
					commitHash = extractTeamCityRunID(result.Metric.TeamCityRunID)
				}

				// Generate a baseline commit hash based on date since we don't have actual baseline data here
				baselineCommitHash := "baseline-" + time.Unix(int64(ts), 0).Format("2006-01-02")

				// Create a ValueJSON with confidence intervals
				benchmark.Values = append(benchmark.Values, ValueJSON{
					CommitDate:           time.Unix(int64(ts), 0),
					CommitHash:           commitHash,
					BaselineCommitHash:   baselineCommitHash, // Use a date-based baseline commit hash
					BaselineCommitDate:   time.Unix(int64(ts), 0),
					BenchmarksCommitHash: "benchmarks",

					Low:    value, // Estimate confidence interval
					Center: value,
					High:   value, // Estimate confidence interval
				})
			}

			// Sort values by commit date
			sort.Slice(benchmark.Values, func(i, j int) bool {
				return benchmark.Values[i].CommitDate.Before(benchmark.Values[j].CommitDate)
			})

			// Add to benchmark data if we have values
			if len(benchmark.Values) > 0 {
				benchmarkData = append(benchmarkData, benchmark)
			}
		}
	}

	if len(benchmarkData) == 0 {
		log.Printf("No benchmark data found for test: %s", testName)
		http.Error(w, fmt.Sprintf("No benchmark data found for test: %s", testName), http.StatusNotFound)
		return
	}

	// Create commits from the benchmarks
	commits := commitsFromBenchmarks(benchmarkData)

	// Return the benchmark data as JSON
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(&SeriesDataJSON{Benchmarks: benchmarkData, Commits: commits}); err != nil {
		log.Printf("Error encoding results: %v", err)
		http.Error(w, "Internal error, see logs", 500)
	}
}

// createBenchmarkComparisons creates benchmark comparisons between current data and baseline data
func createBenchmarkComparisons(currentMetrics map[string][]MetricPoint, baselineMetrics map[string][]MetricPoint) ([]*BenchmarkJSON, error) {
	if len(currentMetrics) == 0 {
		return nil, fmt.Errorf("no current metrics available for comparison")
	}

	if len(baselineMetrics) == 0 {
		return nil, fmt.Errorf("no baseline metrics available for comparison")
	}

	// baselineCommitHash := baselineMetrics["__name__"][0].Labels["commit"]

	log.Printf("Creating benchmark comparisons between %d current metrics and %d baseline metrics",
		len(currentMetrics), len(baselineMetrics))

	// Find metrics that exist in both current and baseline
	var benchmarks []*BenchmarkJSON
	for metricKey, currentPoints := range currentMetrics {
		baselinePoints, hasBaseline := baselineMetrics[metricKey]
		baselineCommitHash := baselinePoints[0].Labels["commit"]
		if !hasBaseline || len(currentPoints) == 0 || len(baselinePoints) == 0 {
			continue // Skip metrics without baseline data or points
		}

		// Ensure points are sorted
		sort.Slice(currentPoints, func(i, j int) bool {
			return currentPoints[i].Timestamp.Before(currentPoints[j].Timestamp)
		})
		sort.Slice(baselinePoints, func(i, j int) bool {
			return baselinePoints[i].Timestamp.Before(baselinePoints[j].Timestamp)
		})

		// Calculate baseline average
		var baselineSum float64
		for _, point := range baselinePoints {
			baselineSum += point.Value
		}
		baselineAvg := baselineSum / float64(len(baselinePoints))

		// Create benchmark with comparisons
		benchmark := &BenchmarkJSON{
			Name:           currentPoints[0].Name,
			Unit:           currentPoints[0].Unit,
			HigherIsBetter: isHigherBetter(currentPoints[0].Unit),
			Values:         make([]ValueJSON, 0, len(currentPoints)),
		}

		// Use a map to ensure only one value per day (using day as key)
		dayValuesMap := make(map[string]ValueJSON)

		// Add comparison for each current point
		for _, point := range currentPoints {
			// Skip points with zero values (would cause division by zero)
			if point.Value == 0 || baselineAvg == 0 {
				continue
			}

			// Calculate ratio and add confidence interval
			ratio := point.Value / baselineAvg

			// Get commit hash from current metric
			commitHash := fmt.Sprintf("%d", point.Timestamp.Unix()) // Fallback to timestamp
			if commit, ok := point.Labels["commit"]; ok && commit != "" {
				commitHash = commit
			} else if runID, ok := point.Labels["test_run_id"]; ok && runID != "" {
				commitHash = extractTeamCityRunID(runID)
			}

			baselineCommitDate := point.Timestamp // Default to current point's timestamp

			// Create the value
			value := ValueJSON{
				CommitDate:           point.Timestamp,
				CommitHash:           commitHash,
				BaselineCommitHash:   baselineCommitHash,
				BaselineCommitDate:   baselineCommitDate,
				BenchmarksCommitHash: "benchmarks",
				Low:                  ratio - 1,
				Center:               ratio - 1,
				High:                 ratio - 1,
			}

			// Use day as the key to avoid duplicates
			dayKey := point.Timestamp.Format("2006-01-02")

			// Only add if we don't already have a value for this day or if this value
			// is later in the day than the one we already have
			if existingValue, exists := dayValuesMap[dayKey]; !exists ||
				point.Timestamp.After(existingValue.CommitDate) {
				dayValuesMap[dayKey] = value
			}
		}

		// Convert map to slice
		for _, value := range dayValuesMap {
			benchmark.Values = append(benchmark.Values, value)
		}

		// Add if we have values
		if len(benchmark.Values) > 0 {
			sort.Slice(benchmark.Values, func(i, j int) bool {
				return benchmark.Values[i].CommitDate.Before(benchmark.Values[j].CommitDate)
			})
			benchmarks = append(benchmarks, benchmark)
		}
	}

	// Calculate regressions for each benchmark
	for _, benchmark := range benchmarks {
		benchmark.Regression = worstRegression(benchmark)
	}

	log.Printf("Created %d benchmark comparisons", len(benchmarks))
	return benchmarks, nil
}

// MetricPoint represents a single data point from a metric
type MetricPoint struct {
	Name      string
	Unit      string
	Labels    map[string]string
	Value     float64
	Timestamp time.Time
}

// extractTeamCityRunID extracts the numeric part from a test_run_id label
func extractTeamCityRunID(teamcityRunID string) string {
	// Remove "teamcity-" prefix if present
	if strings.HasPrefix(teamcityRunID, "teamcity-") {
		return strings.TrimPrefix(teamcityRunID, "teamcity-")
	}
	return teamcityRunID
}

// convertVMDataToMetricPoints converts VictoriaMetrics data to a map of metric points
func convertVMDataToMetricPoints(data []byte) (map[string][]MetricPoint, error) {
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric struct {
					Name           string `json:"__name__"`
					Test           string `json:"test"`
					Unit           string `json:"unit"`
					Branch         string `json:"branch"`
					Cloud          string `json:"cloud"`
					Goos           string `json:"goos"`
					Goarch         string `json:"goarch"`
					TeamCityRunID  string `json:"test_run_id"`
					Commit         string `json:"commit"`
					IsHigherBetter string `json:"is_higher_better"`
				} `json:"metric"`
				Values [][]interface{} `json:"values"` // [timestamp, value] pairs
			} `json:"result"`
		} `json:"data"`
	}

	// Log a preview of the response
	if len(data) == 0 {
		log.Printf("WARNING: Empty VictoriaMetrics response data")
		return map[string][]MetricPoint{}, nil
	}

	if err := json.Unmarshal(data, &response); err != nil {
		log.Printf("Error unmarshaling response: %v", err)
		return nil, fmt.Errorf("error unmarshaling response: %w", err)
	}

	if response.Status != "success" {
		log.Printf("Failed status from VictoriaMetrics: %s", response.Status)
		return nil, fmt.Errorf("unexpected status: %s", response.Status)
	}

	log.Printf("Processing VictoriaMetrics response with %d result series", len(response.Data.Result))

	if len(response.Data.Result) == 0 {
		log.Printf("No results found in VictoriaMetrics response")
		return map[string][]MetricPoint{}, nil
	}

	metrics := make(map[string][]MetricPoint)
	totalPoints := 0

	for _, result := range response.Data.Result {
		// Extract test name and unit from metric
		testName := result.Metric.Test
		if testName == "" {
			continue // Skip metrics without test name
		}

		unit := "ops/s" // default
		if result.Metric.Unit != "" {
			unit = result.Metric.Unit
		}

		metricKey := fmt.Sprintf("%s_%s", testName, unit)
		points := make([]MetricPoint, 0, len(result.Values))

		for _, v := range result.Values {
			if len(v) != 2 {
				continue // Skip invalid data points
			}

			// Extract timestamp and value
			ts, ok := v[0].(float64)
			if !ok {
				continue
			}

			// Parse the value
			var value float64
			switch val := v[1].(type) {
			case string:
				parsedVal, err := strconv.ParseFloat(val, 64)
				if err != nil {
					continue
				}
				value = parsedVal
			case float64:
				value = val
			default:
				continue
			}

			// Create a MetricPoint
			point := MetricPoint{
				Name:      testName,
				Unit:      unit,
				Labels:    createLabelsMap(result.Metric), // Convert struct to map for compatibility
				Value:     value,
				Timestamp: time.Unix(int64(ts), 0),
			}

			points = append(points, point)
			totalPoints++
		}

		if len(points) > 0 {
			// Sort points by timestamp
			sort.Slice(points, func(i, j int) bool {
				return points[i].Timestamp.Before(points[j].Timestamp)
			})

			// Append to existing points or create new entry
			if existingPoints, exists := metrics[metricKey]; exists {
				// Merge the points
				combinedPoints := append(existingPoints, points...)

				// Re-sort the combined points
				sort.Slice(combinedPoints, func(i, j int) bool {
					return combinedPoints[i].Timestamp.Before(combinedPoints[j].Timestamp)
				})

				metrics[metricKey] = combinedPoints
			} else {
				metrics[metricKey] = points
			}
		}
	}

	log.Printf("Processed %d metrics with %d total data points", len(metrics), totalPoints)
	return metrics, nil
}

// createLabelsMap converts a metric struct to a map of labels
func createLabelsMap(metric struct {
	Name           string `json:"__name__"`
	Test           string `json:"test"`
	Unit           string `json:"unit"`
	Branch         string `json:"branch"`
	Cloud          string `json:"cloud"`
	Goos           string `json:"goos"`
	Goarch         string `json:"goarch"`
	TeamCityRunID  string `json:"test_run_id"`
	Commit         string `json:"commit"`
	IsHigherBetter string `json:"is_higher_better"`
}) map[string]string {
	labels := make(map[string]string)
	labels["__name__"] = metric.Name
	labels["test"] = metric.Test
	labels["unit"] = metric.Unit
	labels["branch"] = metric.Branch
	labels["cloud"] = metric.Cloud
	labels["goos"] = metric.Goos
	labels["goarch"] = metric.Goarch
	labels["test_run_id"] = metric.TeamCityRunID
	labels["commit"] = metric.Commit
	labels["is_higher_better"] = metric.IsHigherBetter
	return labels
}

// annotationsHandler serves the annotations YAML file as JSON
func annotationsHandler(w http.ResponseWriter, r *http.Request) {
	// Read the annotations file
	data, err := dashboardFS.ReadFile("dashboard/annotations.yaml")
	if err != nil {
		log.Printf("Error reading annotations file: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Parse the YAML into a map
	var annotations struct {
		Global []struct {
			Date        string `yaml:"date"`
			Description string `yaml:"description"`
			PRs         []int  `yaml:"prs,omitempty"` // Added PRs field
		} `yaml:"global"`
		Tests map[string][]struct {
			Date        string `yaml:"date"`
			Description string `yaml:"description"`
			PRs         []int  `yaml:"prs,omitempty"` // Added PRs field
		} `yaml:"tests"`
	}

	if err := yaml.Unmarshal(data, &annotations); err != nil {
		log.Printf("Error parsing annotations YAML: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Convert the annotations to the format expected by the frontend
	result := struct {
		Global []map[string]interface{}            `json:"global"`
		Tests  map[string][]map[string]interface{} `json:"tests"`
	}{
		Global: make([]map[string]interface{}, 0, len(annotations.Global)),
		Tests:  make(map[string][]map[string]interface{}),
	}

	// Process global annotations
	for _, ann := range annotations.Global {
		annotation := map[string]interface{}{
			"date":        ann.Date,
			"description": ann.Description,
			"color":       "#00FF00", // Bright green color
		}
		if len(ann.PRs) > 0 {
			annotation["prs"] = ann.PRs
		}
		result.Global = append(result.Global, annotation)
	}

	// Process test-specific annotations
	for pattern, anns := range annotations.Tests {
		testAnnotations := make([]map[string]interface{}, 0, len(anns))
		for _, ann := range anns {
			annotation := map[string]interface{}{
				"date":        ann.Date,
				"description": ann.Description,
				"color":       "#00FF00", // Bright green color
			}
			if len(ann.PRs) > 0 {
				annotation["prs"] = ann.PRs
			}
			testAnnotations = append(testAnnotations, annotation)
		}
		result.Tests[pattern] = testAnnotations
	}

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	// Write the response
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("Error encoding annotations response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// getHigherBetter returns whether higher values are better for a metric
// It checks the is_higher_better label and falls back to unit-based heuristics if not available
func getHigherBetter(isHigherBetter string) bool {
	// If the label exists and is parseable as a boolean, use it
	return isHigherBetter == "true"
}
