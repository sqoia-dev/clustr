// Package client provides an HTTP client for clonr CLI → clonr-serverd communication.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/sqoia-dev/clonr/pkg/api"
)

// Client is the HTTP client used by the clonr CLI to talk to clonr-serverd.
type Client struct {
	BaseURL   string
	AuthToken string
	HTTP      *http.Client
}

// New creates a Client with a sensible default timeout.
func New(baseURL, authToken string) *Client {
	return &Client{
		BaseURL:   baseURL,
		AuthToken: authToken,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ListImages returns all BaseImages from the server.
func (c *Client) ListImages(ctx context.Context) ([]api.BaseImage, error) {
	var resp api.ListImagesResponse
	if err := c.get(ctx, "/api/v1/images", &resp); err != nil {
		return nil, err
	}
	return resp.Images, nil
}

// GetImage retrieves a single BaseImage by ID.
func (c *Client) GetImage(ctx context.Context, id string) (*api.BaseImage, error) {
	var img api.BaseImage
	if err := c.get(ctx, "/api/v1/images/"+id, &img); err != nil {
		return nil, err
	}
	return &img, nil
}

// PullImage instructs the server to pull an image from a URL.
// Returns immediately with the image record in "building" status.
func (c *Client) PullImage(ctx context.Context, req api.PullRequest) (*api.BaseImage, error) {
	var img api.BaseImage
	if err := c.post(ctx, "/api/v1/factory/pull", req, &img); err != nil {
		return nil, err
	}
	return &img, nil
}

// GetNodeConfigByMAC retrieves the NodeConfig whose primary_mac matches mac.
func (c *Client) GetNodeConfigByMAC(ctx context.Context, mac string) (*api.NodeConfig, error) {
	var cfg api.NodeConfig
	if err := c.get(ctx, "/api/v1/nodes/by-mac/"+mac, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListNodes returns all NodeConfigs from the server.
func (c *Client) ListNodes(ctx context.Context) ([]api.NodeConfig, error) {
	var resp api.ListNodesResponse
	if err := c.get(ctx, "/api/v1/nodes", &resp); err != nil {
		return nil, err
	}
	return resp.Nodes, nil
}

// GetNode retrieves a single NodeConfig by ID.
func (c *Client) GetNode(ctx context.Context, id string) (*api.NodeConfig, error) {
	var cfg api.NodeConfig
	if err := c.get(ctx, "/api/v1/nodes/"+id, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// DownloadBlob streams the image blob for imageID into w.
// Uses a dedicated http.Client without a read timeout since blobs can be large.
func (c *Client) DownloadBlob(ctx context.Context, imageID string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/api/v1/images/"+imageID+"/blob", nil)
	if err != nil {
		return fmt.Errorf("client: build request: %w", err)
	}
	c.setHeaders(req)

	// Use a no-timeout client for large blobs.
	blobClient := &http.Client{}
	resp, err := blobClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: download blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return c.decodeError(resp)
	}

	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("client: read blob: %w", err)
	}
	return nil
}

// Health checks the server's health endpoint.
func (c *Client) Health(ctx context.Context) (*api.HealthResponse, error) {
	var h api.HealthResponse
	if err := c.get(ctx, "/api/v1/health", &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// SendLogs ships a batch of log entries to POST /api/v1/logs.
func (c *Client) SendLogs(ctx context.Context, entries []api.LogEntry) error {
	return c.post(ctx, "/api/v1/logs", entries, nil)
}

// QueryLogs retrieves historical log entries matching the given filter.
func (c *Client) QueryLogs(ctx context.Context, filter api.LogFilter) ([]api.LogEntry, error) {
	var resp api.ListLogsResponse
	if err := c.get(ctx, buildLogsPath("/api/v1/logs", filter), &resp); err != nil {
		return nil, err
	}
	return resp.Logs, nil
}

// buildLogsPath constructs a query-string path for log endpoints from a filter.
func buildLogsPath(base string, filter api.LogFilter) string {
	q := url.Values{}
	if filter.NodeMAC != "" {
		q.Set("mac", filter.NodeMAC)
	}
	if filter.Hostname != "" {
		q.Set("hostname", filter.Hostname)
	}
	if filter.Level != "" {
		q.Set("level", filter.Level)
	}
	if filter.Component != "" {
		q.Set("component", filter.Component)
	}
	if filter.Since != nil {
		q.Set("since", filter.Since.Format(time.RFC3339))
	}
	if filter.Limit > 0 {
		q.Set("limit", strconv.Itoa(filter.Limit))
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

// ─── Internal helpers ────────────────────────────────────────────────────────

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("client: build request: %w", err)
	}
	c.setHeaders(req)
	return c.do(req, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("client: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("client: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setHeaders(req)
	return c.do(req, out)
}

func (c *Client) setHeaders(req *http.Request) {
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	req.Header.Set("Accept", "application/json")
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return c.decodeError(resp)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("client: decode response: %w", err)
		}
	}
	return nil
}

// decodeError parses an api.ErrorResponse from a non-2xx response.
func (c *Client) decodeError(resp *http.Response) error {
	var errResp api.ErrorResponse
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err != nil || errResp.Error == "" {
		return fmt.Errorf("client: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return fmt.Errorf("client: HTTP %d: %s", resp.StatusCode, errResp.Error)
}
