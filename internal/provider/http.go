package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func defaultHTTP(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 0, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConns: 50, MaxIdleConnsPerHost: 10, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 60 * time.Second}}
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
		// Do not echo arbitrary upstream bodies; they might include credentials or user data.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &APIError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
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
	return out, err
}
func endpoint(base, path string) string { return strings.TrimRight(base, "/") + path }

var errNoUsage = errors.New("provider did not return authoritative usage")
