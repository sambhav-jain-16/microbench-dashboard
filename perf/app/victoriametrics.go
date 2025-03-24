// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// VictoriaMetricsClient represents a client for interacting with VictoriaMetrics.
type VictoriaMetricsClient struct {
	baseURL string
}

// NewVictoriaMetricsClient creates a new VictoriaMetrics client.
func NewVictoriaMetricsClient(baseURL string) *VictoriaMetricsClient {
	return &VictoriaMetricsClient{
		baseURL: baseURL,
	}
}

// Query executes a PromQL query against VictoriaMetrics.
func (c *VictoriaMetricsClient) Query(ctx context.Context, query string, start, end time.Time) ([]byte, error) {
	// Construct the query URL
	url := fmt.Sprintf("%s/api/v1/query_range", c.baseURL)

	// Create the request
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	// Add query parameters
	q := req.URL.Query()
	q.Add("query", query)
	q.Add("start", fmt.Sprintf("%d", start.Unix()))
	q.Add("end", fmt.Sprintf("%d", end.Unix()))
	q.Add("step", "1h") // 1 hour resolution
	req.URL.RawQuery = q.Encode()

	// Make the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error executing query: %w", err)
	}
	defer resp.Body.Close()

	// Read the response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// latestTimestamp returns the timestamp of the latest data point in VictoriaMetrics.
func (c *VictoriaMetricsClient) latestTimestamp(ctx context.Context) (time.Time, error) {
	// Query for the latest timestamp
	query := `max_over_time(benchmark_result{measurement="benchmark-result"}[30d])`
	end := time.Now()
	start := end.Add(-30 * 24 * time.Hour) // 30 days

	data, err := c.Query(ctx, query, start, end)
	if err != nil {
		return time.Time{}, fmt.Errorf("error querying latest timestamp: %w", err)
	}

	// Parse the response to get the timestamp
	var response struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []interface{} `json:"value"` // [timestamp, value]
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.Unmarshal(data, &response); err != nil {
		return time.Time{}, fmt.Errorf("error parsing response: %w", err)
	}

	if len(response.Data.Result) == 0 || len(response.Data.Result[0].Value) < 2 {
		return time.Time{}, fmt.Errorf("no data found")
	}

	// The timestamp is the first element in the value array
	timestamp, ok := response.Data.Result[0].Value[0].(float64)
	if !ok {
		return time.Time{}, fmt.Errorf("invalid timestamp format")
	}

	return time.Unix(int64(timestamp), 0), nil
}
