// Package arianet is the official Go client for the Arianet (Ariaservice) public API.
//
//	client := arianet.NewClient("") // reads ARIANET_TOKEN
//	resp, err := client.Cloud.ListServers(ctx, nil)
//
// Every API group (Core, Cloud, RegionalVPS, Hosting, Baremetal, SMS) is a field of
// Client with one method per operation of its OpenAPI spec. Methods return the raw
// *Response; decode it into your own types with Response.Decode.
package arianet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the public API gateway.
	DefaultBaseURL = "https://api.ariaservice.net"
	// EnvToken is read when NewClient gets an empty token.
	EnvToken = "ARIANET_TOKEN"
	// EnvBaseURL overrides DefaultBaseURL when set.
	EnvBaseURL = "ARIANET_API_URL"
	// RegionHeader names the regional install of a product API a request is for.
	RegionHeader = "Arianet-Region"

	defaultTimeout      = 60 * time.Second
	defaultMaxRetries   = 3
	defaultMaxRetryWait = 60 * time.Second
	maxResponseBytes    = 32 << 20
)

// Client calls the Arianet API with a personal API token.
type Client struct {
	Core        *CoreService
	Cloud       *CloudService
	RegionalVPS *RegionalVPSService
	Hosting     *HostingService
	Baremetal   *BaremetalService
	SMS         *SMSService

	baseURL      string
	token        string
	httpClient   *http.Client
	userAgent    string
	maxRetries   int
	maxRetryWait time.Duration
	sleep        func(context.Context, time.Duration) error
	err          error
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL sets the API root (default https://api.ariaservice.net, or
// $ARIANET_API_URL). It must be https, except for localhost.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient replaces the underlying *http.Client. Its redirect policy is
// replaced so the token never follows a redirect to another host.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.httpClient = hc } }

// WithMaxRetries sets how many times a request is repeated after a 429, or
// after a network error or 502/503/504 when the request is safe to repeat. Default 3.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// WithMaxRetryWait bounds how long the client waits before one retry. A 429 whose
// Retry-After is longer is returned as an error instead. Default 60s.
func WithMaxRetryWait(d time.Duration) Option { return func(c *Client) { c.maxRetryWait = d } }

// WithUserAgent appends a product token to the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = strings.TrimSpace(c.userAgent + " " + ua) }
}

// NewClient returns a client authenticated with token, or with $ARIANET_TOKEN
// when token is empty. Create tokens at https://cloud.ariaservice.net/users/api-tokens.
func NewClient(token string, opts ...Option) *Client {
	if token == "" {
		token = os.Getenv(EnvToken)
	}
	c := &Client{
		baseURL:      DefaultBaseURL,
		token:        token,
		userAgent:    "arianet-go/" + Version,
		maxRetries:   defaultMaxRetries,
		maxRetryWait: defaultMaxRetryWait,
		sleep:        sleepContext,
	}
	if env := os.Getenv(EnvBaseURL); env != "" {
		c.baseURL = env
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: defaultTimeout}
	}
	hc := *c.httpClient
	hc.CheckRedirect = refuseRedirect
	c.httpClient = &hc
	c.baseURL = strings.TrimRight(c.baseURL, "/")
	c.err = checkBaseURL(c.baseURL)
	c.initServices()
	return c
}

func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("arianet: invalid base URL %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if u.Scheme == "http" && (host == "localhost" || net.ParseIP(host).IsLoopback()) {
		return nil
	}
	return fmt.Errorf("arianet: base URL %q must use https", raw)
}

// refuseRedirect never follows a redirect: the API does not redirect, and
// following one could send the token somewhere else.
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// RequestOption adjusts one request.
type RequestOption func(*requestConfig)

type requestConfig struct {
	headers        http.Header
	idempotencyKey string
	region         string
}

// WithIdempotencyKey sets the Idempotency-Key header. Requests with a key are
// also retried after a network error or a 5xx, since the API answers a repeat
// with the original result.
func WithIdempotencyKey(key string) RequestOption {
	return func(rc *requestConfig) { rc.idempotencyKey = key }
}

// WithHeader adds a header to one request.
func WithHeader(name, value string) RequestOption {
	return func(rc *requestConfig) { rc.headers.Set(name, value) }
}

// WithRegion sends the request to one regional install of a product API.
func WithRegion(region string) RequestOption {
	return func(rc *requestConfig) { rc.region = region }
}

// Do sends a request to any API path (including the product prefix, e.g.
// "/cloud/v1/servers"). Generated methods call it; use it directly for
// endpoints this SDK version does not have yet. body is encoded as JSON.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, opts ...RequestOption) (*Response, error) {
	return c.do(ctx, method, path, query, body, false, "", opts)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, idempotent bool, region string, opts []RequestOption) (*Response, error) {
	if c.err != nil {
		return nil, c.err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rc := &requestConfig{headers: http.Header{}, region: region}
	for _, opt := range opts {
		opt(rc)
	}
	if idempotent && rc.idempotencyKey == "" && method != http.MethodGet {
		rc.idempotencyKey = newIdempotencyKey()
	}

	var payload []byte
	if body != nil {
		switch b := body.(type) {
		case []byte:
			payload = b
		case json.RawMessage:
			payload = b
		default:
			var err error
			if payload, err = json.Marshal(body); err != nil {
				return nil, fmt.Errorf("arianet: encode request body: %w", err)
			}
		}
	}

	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}
	// A transport error or a 5xx is only repeated when doing so cannot
	// perform the operation twice.
	safe := method == http.MethodGet || method == http.MethodHead || rc.idempotencyKey != ""

	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, method, fullURL, payload, rc)
		if err == nil {
			return resp, nil
		}
		if attempt >= c.maxRetries || ctx.Err() != nil {
			return nil, err
		}
		wait, ok := c.retryWait(err, attempt, safe)
		if !ok {
			return nil, err
		}
		if serr := c.sleep(ctx, wait); serr != nil {
			return nil, err
		}
	}
}

func (c *Client) attempt(ctx context.Context, method, fullURL string, payload []byte, rc *requestConfig) (*Response, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, fmt.Errorf("arianet: build request: %w", err)
	}
	for k, vs := range rc.headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if rc.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", rc.idempotencyKey)
	}
	if rc.region != "" {
		req.Header.Set(RegionHeader, rc.region)
	}

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &ConnectionError{Err: err}
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, maxResponseBytes))
	if err != nil {
		return nil, &ConnectionError{Err: err}
	}
	resp := &Response{StatusCode: httpResp.StatusCode, Header: httpResp.Header, Body: raw}
	if apiErr := errorFromResponse(resp); apiErr != nil {
		return nil, apiErr
	}
	return resp, nil
}

// retryWait says whether a failed attempt is repeated and after how long.
func (c *Client) retryWait(err error, attempt int, safe bool) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusTooManyRequests:
			wait := apiErr.RetryAfter
			if wait <= 0 {
				wait = backoff(attempt)
			}
			return wait, wait <= c.maxRetryWait
		case safe && (apiErr.StatusCode == http.StatusBadGateway || apiErr.StatusCode == http.StatusServiceUnavailable || apiErr.StatusCode == http.StatusGatewayTimeout):
			wait := apiErr.RetryAfter
			if wait <= 0 {
				wait = backoff(attempt)
			}
			return wait, wait <= c.maxRetryWait
		}
		return 0, false
	}
	var connErr *ConnectionError
	if safe && errors.As(err, &connErr) {
		return backoff(attempt), true
	}
	return 0, false
}

// backoff is 0.5s, 1s, 2s, 4s, capped at 8s, plus up to 25% jitter.
func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << uint(attempt)
	if d > 8*time.Second || d <= 0 {
		d = 8 * time.Second
	}
	jitter, _ := rand.Int(rand.Reader, big.NewInt(int64(d/4)+1))
	return d + time.Duration(jitter.Int64())
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseRetryAfter reads Retry-After (seconds or an HTTP date), or on a 429
// without it RateLimit-Reset (seconds until the window resets).
func parseRetryAfter(h http.Header, status int) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
			return 0
		}
	}
	if status == http.StatusTooManyRequests {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Get("RateLimit-Reset"))); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

func newIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
