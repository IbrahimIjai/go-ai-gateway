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
	"sync/atomic"
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

// defaultStreamIdle is how long a stream may go without upstream bytes.
const defaultStreamIdle = 120 * time.Second

var errStreamIdle = fmt.Errorf("upstream stream idle: %w", context.DeadlineExceeded)

// openStream runs send under its own cancellable context and wraps the body so
// a silent upstream is cut off after idle (zero means defaultStreamIdle).
func openStream(ctx context.Context, idle time.Duration, send func(context.Context) (*http.Response, error)) (io.ReadCloser, error) {
	ctx, cancel := context.WithCancel(ctx)
	resp, err := send(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	if idle <= 0 {
		idle = defaultStreamIdle
	}
	return newIdleReader(resp.Body, idle, cancel), nil
}

// idleReader cancels its request when no bytes arrive for timeout. Cancelling
// the request context is what makes net/http abort a Read blocked on a silent
// connection; closing the body from another goroutine would wait for it.
type idleReader struct {
	body    io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	fired   atomic.Bool
	cancel  context.CancelFunc
}

func newIdleReader(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleReader {
	r := &idleReader{body: body, timeout: timeout, cancel: cancel}
	r.timer = time.AfterFunc(timeout, func() {
		r.fired.Store(true)
		cancel()
	})
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if err != nil && r.fired.Load() {
		return n, errStreamIdle
	}
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (r *idleReader) Close() error {
	r.timer.Stop()
	r.cancel()
	return r.body.Close()
}
