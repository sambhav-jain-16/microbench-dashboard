// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	q.Add("step", "1d") // 1 day resolution
	req.URL.RawQuery = q.Encode()

	// Log the full query URL for debugging
	fullURL := req.URL.String()
	fmt.Printf("VictoriaMetrics query URL: %s\n", fullURL)
	log.Printf("Executing VictoriaMetrics query: %s", fullURL)
	log.Printf("Time range: %s to %s", start.Format(time.RFC3339), end.Format(time.RFC3339))

	// Make the request
	client := &http.Client{
		Timeout: 30 * time.Second, // Set a reasonable timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("ERROR: Failed to execute VictoriaMetrics query: %v", err)
		return nil, fmt.Errorf("error executing query: %w", err)
	}
	defer resp.Body.Close()

	// Read the response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("ERROR: Failed to read VictoriaMetrics response: %v", err)
		return nil, fmt.Errorf("error reading response: %w", err)
	}

	// Check if the response body is empty
	if len(body) == 0 {
		log.Printf("ERROR: Empty response body from VictoriaMetrics")
		return nil, fmt.Errorf("empty response body from VictoriaMetrics")
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("ERROR: VictoriaMetrics returned status code %d: %s", resp.StatusCode, string(body))
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	// Log the raw response for debugging (truncated to avoid overwhelming logs)
	responsePreview := string(body)
	if len(responsePreview) > 500 {
		responsePreview = responsePreview[:500] + "... [truncated]"
	}
	log.Printf("VictoriaMetrics response (preview): %s", responsePreview)

	// Check if the result is empty but successful
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string        `json:"resultType"`
			Result     []interface{} `json:"result"`
		} `json:"data"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		log.Printf("WARNING: Could not parse VictoriaMetrics response: %v", err)
		log.Printf("Response body: %s", responsePreview)
		// Continue anyway since we'll return the raw body
	} else {
		// Check for error in the response
		if response.Status == "error" {
			log.Printf("ERROR: VictoriaMetrics query failed: %s (%s)", response.Error, response.ErrorType)
			// We'll continue and return the error body for more context
		}

		log.Printf("Response status: %s, result type: %s, result count: %d",
			response.Status, response.Data.ResultType, len(response.Data.Result))

		if response.Status == "success" && len(response.Data.Result) == 0 {
			log.Printf("WARNING: Query returned successfully but with empty results. Query: %s", query)
			log.Printf("Time range: %s to %s", start.Format(time.RFC3339), end.Format(time.RFC3339))

			// Try to extract more information about why the query returned no results
			checkURL := fmt.Sprintf("%s/api/v1/label/test/values", c.baseURL)
			checkReq, err := http.NewRequestWithContext(ctx, "GET", checkURL, nil)
			if err == nil {
				checkResp, err := client.Do(checkReq)
				if err == nil && checkResp.StatusCode == http.StatusOK {
					defer checkResp.Body.Close()
					checkBody, err := io.ReadAll(checkResp.Body)
					if err == nil {
						var checkResponse struct {
							Status string   `json:"status"`
							Data   []string `json:"data"`
						}
						if err := json.Unmarshal(checkBody, &checkResponse); err == nil && len(checkResponse.Data) > 0 {
							log.Printf("Available tests in VictoriaMetrics (first 10): %v", checkResponse.Data[:min(10, len(checkResponse.Data))])
						}
					}
				}
			}

			// Also check available metrics
			metricsURL := fmt.Sprintf("%s/api/v1/label/__name__/values", c.baseURL)
			metricsReq, err := http.NewRequestWithContext(ctx, "GET", metricsURL, nil)
			if err == nil {
				metricsResp, err := client.Do(metricsReq)
				if err == nil && metricsResp.StatusCode == http.StatusOK {
					defer metricsResp.Body.Close()
					metricsBody, err := io.ReadAll(metricsResp.Body)
					if err == nil {
						var metricsResponse struct {
							Status string   `json:"status"`
							Data   []string `json:"data"`
						}
						if err := json.Unmarshal(metricsBody, &metricsResponse); err == nil && len(metricsResponse.Data) > 0 {
							log.Printf("Available metrics in VictoriaMetrics (first 10): %v", metricsResponse.Data[:min(10, len(metricsResponse.Data))])
						}
					}
				}
			}
		}
	}

	return body, nil
}

// min returns the smaller of x or y.
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}
