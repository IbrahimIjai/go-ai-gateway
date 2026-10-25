package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewHTTPClient returns a client for one upstream provider. Reuse it for every
// request to that provider: its transport owns the keep-alive pool.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// A non-streaming completion sends its headers only once the whole
		// answer exists, so this bounds generation time, not network latency.
		ResponseHeaderTimeout: 10 * time.Minute,
	}}
}

// sharedClient serves adapters built without a Client, so they still reuse
// connections instead of building a new transport per request.
var sharedClient = NewHTTPClient()

func defaultHTTP(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return sharedClient
}
func postJSON(ctx context.Context, c *http.Client, url string, body any, headers map[string]string) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	resp, err := c.Do(request)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		// Read only to classify the failure. Never echo or log upstream
		// bodies; they might include credentials or user data.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, newAPIError(resp, body, time.Now())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected upstream status %d", resp.StatusCode)
	}
	return resp, nil
}
func readJSON[T any](resp *http.Response) (T, error) {
	defer resp.Body.Close()
	var out T
	err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out)
	// Drain what the decoder left (a newline or chunk terminator) so the
	// connection can return to the pool.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return out, err
}
func endpoint(base, path string) string { return strings.TrimRight(base, "/") + path }
