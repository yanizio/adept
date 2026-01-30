// internal/api/client.go
//
// Package api provides reusable HTTP helpers with retries, rate limiting,
// and JSON marshaling.  It stays provider-agnostic so service clients can
// share retry, telemetry, and auth patterns.
//
// Oxford commas, two spaces after periods.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// Options configures the shared HTTP client wrapper.
type Options struct {
	Timeout            time.Duration
	RequestsPerSecond  float64
	Burst              int
	Retries            int
	RetryBackoff       time.Duration
	DefaultHeaders     map[string]string
	RetryStatusCodes   map[int]bool
	RetryMethods       map[string]bool
	MaxResponseBodyLen int64
}

// Client wraps http.Client with retry and rate limiting.
type Client struct {
	httpClient *http.Client
	limiter    *rate.Limiter
	opts       Options
}

const defaultMaxBodyLen = 1 << 20 // 1MB

// New returns a configured Client with sensible defaults.
func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.RequestsPerSecond == 0 {
		opts.RequestsPerSecond = 20
	}
	if opts.Burst == 0 {
		opts.Burst = 20
	}
	if opts.Retries == 0 {
		opts.Retries = 2
	}
	if opts.RetryBackoff == 0 {
		opts.RetryBackoff = 200 * time.Millisecond
	}
	if opts.RetryStatusCodes == nil {
		opts.RetryStatusCodes = map[int]bool{
			http.StatusTooManyRequests:    true,
			http.StatusBadGateway:         true,
			http.StatusServiceUnavailable: true,
			http.StatusGatewayTimeout:     true,
		}
	}
	if opts.RetryMethods == nil {
		opts.RetryMethods = map[string]bool{
			http.MethodGet:  true,
			http.MethodPost: true,
			http.MethodPut:  true,
		}
	}
	if opts.MaxResponseBodyLen == 0 {
		opts.MaxResponseBodyLen = defaultMaxBodyLen
	}
	return &Client{
		httpClient: &http.Client{Timeout: opts.Timeout},
		limiter:    rate.NewLimiter(rate.Limit(opts.RequestsPerSecond), opts.Burst),
		opts:       opts,
	}
}

// DoJSON sends an HTTP request with a JSON body and unmarshals the response.
func (c *Client) DoJSON(ctx context.Context, method, url string, payload any, dst any, headers map[string]string) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("api: json marshal: %w", err)
	}
	return c.doWithRetry(ctx, method, url, body, dst, headers)
}

func (c *Client) doWithRetry(ctx context.Context, method, url string, body []byte, dst any, headers map[string]string) (int, error) {
	attempts := c.opts.Retries + 1
	backoff := c.opts.RetryBackoff
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := c.waitRate(ctx); err != nil {
			return 0, err
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return 0, fmt.Errorf("api: build request: %w", err)
		}
		applyHeaders(req, c.opts.DefaultHeaders)
		applyHeaders(req, headers)
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}

		status, err := c.doOnce(req, dst)
		if err == nil {
			return status, nil
		}
		lastErr = err
		if !c.shouldRetry(req.Method, status, err) || i == attempts-1 {
			return status, err
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
		}
	}
	if lastErr == nil {
		lastErr = errors.New("api: request failed")
	}
	return 0, lastErr
}

func (c *Client) doOnce(req *http.Request, dst any) (int, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxResponseBodyLen))
		return resp.StatusCode, Error{StatusCode: resp.StatusCode, Body: string(payload)}
	}
	if dst == nil {
		return resp.StatusCode, nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, c.opts.MaxResponseBodyLen))
	if err := dec.Decode(dst); err != nil {
		return resp.StatusCode, fmt.Errorf("api: decode response: %w", err)
	}
	return resp.StatusCode, nil
}

func (c *Client) shouldRetry(method string, status int, err error) bool {
	if err == nil {
		return false
	}
	if !c.opts.RetryMethods[method] {
		return false
	}
	if status == 0 {
		return true
	}
	return c.opts.RetryStatusCodes[status]
}

func (c *Client) waitRate(ctx context.Context) error {
	if c.limiter == nil {
		return nil
	}
	return c.limiter.Wait(ctx)
}

func applyHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
}
