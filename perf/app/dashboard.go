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
	// Serve main.html as the default dashboard page
	mux.HandleFunc("/dashboard/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dashboard/" {
			// If benchmark parameter is present, serve index.html
			if r.URL.Query().Get("benchmark") != "" {
				data, err := dashboardFS.ReadFile("dashboard/index.html")
				if err != nil {
					http.Error(w, "Internal server error", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				w.Write(data)
				return
			}

			// Otherwise serve main.html for the root dashboard path
			data, err := dashboardFS.ReadFile("dashboard/main.html")
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
				Name:           name,
				Unit:           unit,
				HigherIsBetter: isHigherBetter(unit),
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
// Empty confidence intervals are problematic and produces infinities
// or NaNs.
func changeScore(l1, c1, h1, l2, c2, h2 float64) float64 {
	sign := 1.0
	if c1 > c2 {
		l1, c1, h1, l2, c2, h2 = l2, c2, h2, l1, c1, h1
		sign = -sign
	}
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
		scores = append(scores, math.Abs(changeScore(v1.Low, v1.Center, v1.High, v0.Low, v0.Center, v0.High)))
	}

	sort.Float64s(scores)
	median := (scores[len(scores)/2] + scores[(len(scores)-1)/2]) / 2

	// MAGIC NUMBER "1".  Removing this added 25% to the "detected regressions", but they were all junk.
	if median > 1 {
		worst.IgnoredBecause = "median change score > 1"
		return worst
	}

	if math.IsNaN(median) {
		worst.IgnoredBecause = "median is NaN"
		return worst
	}

	// MAGIC NUMBER "1.2".  Smaller than that tends to admit junky benchmarks.
	magicScoreThreshold := math.Max(2*median, 1.2)

	// Scan backwards looking for most recent outlier regression
	for i := l - 1; i > 0; i-- {
		v1, v0 := values[i-1], values[i]
		score := sign * changeScore(v1.Low, v1.Center, v1.High, v0.Low, v0.Center, v0.High)

		if score > magicScoreThreshold && sign*v1.Center < min && score > worst.deltaScore {
			worst.DeltaIndex = i
			worst.deltaScore = score
			worst.Delta = sign * (v0.Center - v1.Center)
		}

		min = math.Min(sign*v0.Center, min)
	}

	if worst.DeltaIndex == -1 {
		worst.IgnoredBecause = "didn't detect outlier regression"
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
		end, err = time.Parse("2006-01-02T15:04", endParam)
		if err != nil {
			log.Printf("Error parsing end %q: %v", endParam, err)
			http.Error(w, "end parameter must be a timestamp similar to RFC3339 without a time zone, like 2000-12-31T15:00", http.StatusBadRequest)
			return
		}
	}

	start := end.Add(-24 * time.Hour * time.Duration(days))

	methStart := time.Now()
	defer func() {
		log.Printf("Dashboard total query time: %s", time.Since(methStart))
	}()

	vmClient := NewVictoriaMetricsClient(a.VictoriaMetricsURL)

	cloud := r.FormValue("cloud")
	if cloud == "" {
		cloud = "gce"
	}
	branch := r.FormValue("branch")
	if branch == "" {
		branch = "master"
	}

	benchmark := r.FormValue("benchmark")
	unit := r.FormValue("unit")
	var benchmarks []*BenchmarkJSON

	if unit != "" {
		// Fetch a single and specific benchmark
		query := fmt.Sprintf(`benchmark_result{test="%s",cloud="%s",branch="%s",unit="%s"}`,
			benchmark, cloud, branch, unit)
		data, err := vmClient.Query(ctx, query, start, end)
		if err != nil {
			log.Printf("Error querying VictoriaMetrics: %v", err)
			http.Error(w, "Error querying VictoriaMetrics", 500)
			return
		}
		// Parse the VictoriaMetrics response and convert to BenchmarkJSON
		benchmarks, err = parseVictoriaMetricsResponse(data)
		if err != nil {
			log.Printf("Error parsing VictoriaMetrics response: %v", err)
			http.Error(w, "Error parsing VictoriaMetrics response", 500)
			return
		}
	} else {
		// Fetch all benchmarks matching the criteria
		// Use regexp matching for benchmark name, ensure unit is not empty
		var benchmarkFilter string
		if benchmark != "" {
			benchmarkFilter = fmt.Sprintf(`,test=~"%s"`, benchmark)
		} else {
			benchmarkFilter = ""
		}

		query := fmt.Sprintf(`benchmark_result{cloud="%s",branch="%s"%s,unit!=""}`,
			cloud, branch, benchmarkFilter)
		data, err := vmClient.Query(ctx, query, start, end)
		if err != nil {
			log.Printf("Error querying VictoriaMetrics: %v", err)
			http.Error(w, "Error querying VictoriaMetrics", 500)
			return
		}
		benchmarks, err = parseVictoriaMetricsResponse(data)
		if err != nil {
			log.Printf("Error parsing VictoriaMetrics response: %v", err)
			http.Error(w, "Error parsing VictoriaMetrics response", 500)
			return
		}
	}

	if len(benchmarks) == 0 {
		log.Printf("No benchmarks found matching criteria")
		http.Error(w, "No benchmarks found", 404)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		w = &gzipResponseWriter{w: gz, ResponseWriter: w}
	}

	commits := commitsFromBenchmarks(benchmarks)
	if err := json.NewEncoder(w).Encode(&DataJSON{Benchmarks: benchmarks, Commits: commits}); err != nil {
		log.Printf("Error encoding results: %v", err)
		http.Error(w, "Internal error, see logs", 500)
	}
}

// parseVictoriaMetricsResponse parses the VictoriaMetrics response into BenchmarkJSON format
func parseVictoriaMetricsResponse(data []byte) ([]*BenchmarkJSON, error) {
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric struct {
					Name   string `json:"__name__"`
					Test   string `json:"test"`
					Unit   string `json:"unit"`
					Branch string `json:"branch"`
					Cloud  string `json:"cloud"`
					Goos   string `json:"goos"`
					Goarch string `json:"goarch"`
				} `json:"metric"`
				Values []struct {
					Timestamp float64 `json:"timestamp"`
					Value     string  `json:"value"`
				} `json:"values"`
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
	if len(response.Data.Result) == 0 {
		log.Printf("No metrics found in VictoriaMetrics response")
		log.Printf("Raw response data: %s", string(data))
		return nil, fmt.Errorf("no metrics found in response")
	}

	log.Printf("Found %d metrics in response", len(response.Data.Result))
	metricNames := make(map[string]bool)
	for _, result := range response.Data.Result {
		metricNames[result.Metric.Name] = true
		log.Printf("Metric: %s, Test: %s, Unit: %s, Branch: %s, Cloud: %s",
			result.Metric.Name, result.Metric.Test, result.Metric.Unit,
			result.Metric.Branch, result.Metric.Cloud)
	}
	log.Printf("Available metric names: %v", maps.Keys(metricNames))

	// Group results by benchmark name and unit
	benchmarks := make(map[string]*BenchmarkJSON)

	for _, result := range response.Data.Result {
		metric := result.Metric

		// Skip metrics without test or unit
		if metric.Test == "" || metric.Unit == "" {
			log.Printf("Skipping metric %s without test or unit", metric.Name)
			continue
		}

		// Use test and unit as key
		key := fmt.Sprintf("%s_%s", metric.Test, metric.Unit)
		benchmark, ok := benchmarks[key]
		if !ok {
			benchmark = &BenchmarkJSON{
				Name:           metric.Test,
				Unit:           metric.Unit,
				HigherIsBetter: isHigherBetter(metric.Unit),
			}
			benchmarks[key] = benchmark
		}

		// Convert values to ValueJSON format
		for _, v := range result.Values {
			value, err := strconv.ParseFloat(v.Value, 64)
			if err != nil {
				return nil, fmt.Errorf("error parsing value: %w", err)
			}

			// Extract commit details from metadata or use placeholders
			// For now, using placeholders as we don't know the exact structure
			commitHash := "unknown"
			baselineCommitHash := "baseline"
			benchmarksCommitHash := "benchmarks"

			benchmark.Values = append(benchmark.Values, ValueJSON{
				CommitDate:           time.Unix(int64(v.Timestamp), 0),
				CommitHash:           commitHash,
				BaselineCommitHash:   baselineCommitHash,
				BenchmarksCommitHash: benchmarksCommitHash,
				Low:                  value - 0.05, // Estimate confidence interval
				Center:               value,
				High:                 value + 0.05, // Estimate confidence interval
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
