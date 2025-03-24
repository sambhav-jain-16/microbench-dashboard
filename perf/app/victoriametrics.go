package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// VictoriaMetricsClient handles communication with VictoriaMetrics
type VictoriaMetricsClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewVictoriaMetricsClient creates a new VictoriaMetrics client
func NewVictoriaMetricsClient(baseURL string) *VictoriaMetricsClient {
	return &VictoriaMetricsClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Query executes a PromQL query against VictoriaMetrics
func (c *VictoriaMetricsClient) Query(ctx context.Context, query string, start, end time.Time) ([]byte, error) {
	url := fmt.Sprintf("%s/api/v1/query_range", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %w", err)
	}

	q := req.URL.Query()
	q.Add("query", query)
	q.Add("start", start.Format(time.RFC3339))
	q.Add("end", end.Format(time.RFC3339))
	q.Add("step", "1h") // Adjust step size as needed
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error executing query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}
