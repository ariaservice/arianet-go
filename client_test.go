package arianet

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, path, query, auth, ua, idem, region, body string
}

// mockServer answers each request with the next scripted reply and records it.
type mockServer struct {
	t       *testing.T
	mu      sync.Mutex
	replies []func(w http.ResponseWriter)
	seen    []recorded
	srv     *httptest.Server
}

func newMock(t *testing.T, replies ...func(w http.ResponseWriter)) *mockServer {
	m := &mockServer{t: t, replies: replies}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.seen = append(m.seen, recorded{
			method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery,
			auth: r.Header.Get("Authorization"), ua: r.Header.Get("User-Agent"),
			idem: r.Header.Get("Idempotency-Key"), region: r.Header.Get(RegionHeader), body: string(b),
		})
		n := len(m.seen)
		m.mu.Unlock()
		if n > len(m.replies) {
			t.Errorf("unexpected request %d: %s %s", n, r.Method, r.URL)
			w.WriteHeader(500)
			return
		}
		m.replies[n-1](w)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func reply(status int, body string, headers ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// newTestClient records the waits instead of sleeping.
func newTestClient(m *mockServer, opts ...Option) (*Client, *[]time.Duration) {
	waits := &[]time.Duration{}
	c := NewClient("tok_123", append([]Option{WithBaseURL(m.srv.URL)}, opts...)...)
	c.sleep = func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	return c, waits
}

func TestAuthHeadersAndPath(t *testing.T) {
	m := newMock(t, reply(200, `{"data":[{"id":1}],"meta":{}}`))
	c, _ := newTestClient(m)
	resp, err := c.Cloud.ListServers(context.Background(), &CloudListServersParams{Page: Int(2), LabelSelector: String("env=prod")})
	if err != nil {
		t.Fatal(err)
	}
	got := m.seen[0]
	if got.auth != "Bearer tok_123" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if !strings.HasPrefix(got.ua, "arianet-go/") {
		t.Errorf("User-Agent = %q", got.ua)
	}
	if got.method != "GET" || got.path != "/cloud/v1/servers" {
		t.Errorf("request = %s %s", got.method, got.path)
	}
	if got.query != "label_selector=env%3Dprod&page=2" {
		t.Errorf("query = %q", got.query)
	}
	var servers []struct{ ID int }
	if err := resp.DecodeData(&servers); err != nil || len(servers) != 1 || servers[0].ID != 1 {
		t.Errorf("DecodeData = %v, %v", servers, err)
	}
}

func TestTokenFromEnvironment(t *testing.T) {
	t.Setenv(EnvToken, "env_tok")
	m := newMock(t, reply(200, `{"success":true,"data":{}}`))
	c := NewClient("", WithBaseURL(m.srv.URL))
	if _, err := c.Core.GetMe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.seen[0].auth != "Bearer env_tok" {
		t.Errorf("Authorization = %q", m.seen[0].auth)
	}
}

func TestPathParamsAreEscaped(t *testing.T) {
	m := newMock(t, reply(200, `{}`))
	c, _ := newTestClient(m)
	if _, err := c.Core.GetInvoice(context.Background(), "INV/1 2"); err != nil {
		t.Fatal(err)
	}
	if m.seen[0].path != "/api/v1/invoices/INV%2F1%202" {
		t.Errorf("path = %q", m.seen[0].path)
	}
}

func TestRetryOn429HonorsRetryAfter(t *testing.T) {
	m := newMock(t,
		reply(429, `{"code":"rate_limited","message":"slow down"}`, "Retry-After", "7"),
		reply(200, `{"data":[]}`),
	)
	c, waits := newTestClient(m)
	// A POST is retried on 429 too: the request was refused, not performed.
	if _, err := c.SMS.SendMessage(context.Background(), map[string]any{"to": []string{"0912"}}); err != nil {
		t.Fatal(err)
	}
	if len(m.seen) != 2 || len(*waits) != 1 || (*waits)[0] != 7*time.Second {
		t.Fatalf("requests = %d, waits = %v", len(m.seen), *waits)
	}
	if m.seen[1].body != m.seen[0].body {
		t.Errorf("retry body %q != %q", m.seen[1].body, m.seen[0].body)
	}
}

func TestRetryOn429UsesRateLimitReset(t *testing.T) {
	m := newMock(t, reply(429, `{}`, "RateLimit-Reset", "3"), reply(200, `{}`))
	c, waits := newTestClient(m)
	if _, err := c.Cloud.ListDatacenters(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 1 || (*waits)[0] != 3*time.Second {
		t.Errorf("waits = %v", *waits)
	}
}

func TestRetryAfterLongerThanMaxIsReturned(t *testing.T) {
	m := newMock(t, reply(429, `{"success":false,"error":{"code":"RATE_LIMIT_EXCEEDED","message":"Too many"}}`, "Retry-After", "3600"))
	c, waits := newTestClient(m)
	_, err := c.Core.ListRegions(context.Background(), nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || apiErr.RetryAfter != time.Hour {
		t.Fatalf("err = %#v", err)
	}
	if apiErr.Code != ErrorCodeRateLimitExceeded || !IsRateLimited(err) {
		t.Errorf("code = %q", apiErr.Code)
	}
	if len(*waits) != 0 || len(m.seen) != 1 {
		t.Errorf("retried: waits=%v requests=%d", *waits, len(m.seen))
	}
}

func TestRetriesAreBounded(t *testing.T) {
	m := newMock(t, reply(503, `{}`), reply(503, `{}`), reply(503, `{}`))
	c, _ := newTestClient(m, WithMaxRetries(2))
	_, err := c.Cloud.ListVolumes(context.Background(), nil)
	if StatusCode(err) != 503 || len(m.seen) != 3 {
		t.Fatalf("err = %v, requests = %d", err, len(m.seen))
	}
}

func TestUnsafeRequestNotRetriedOn503(t *testing.T) {
	m := newMock(t, reply(503, `{"error":"busy","code":"core_unavailable"}`))
	c, _ := newTestClient(m)
	_, err := c.Cloud.CreateVolume(context.Background(), map[string]any{"size": 10})
	if !HasCode(err, ErrorCodeCoreUnavailable) || len(m.seen) != 1 {
		t.Fatalf("err = %v, requests = %d", err, len(m.seen))
	}
}

func TestIdempotentOperationKeepsKeyAcrossRetries(t *testing.T) {
	m := newMock(t, reply(502, `{}`), reply(202, `{"success":true,"data":{"id":9}}`))
	c, _ := newTestClient(m)
	if _, err := c.Core.CreateServer(context.Background(), map[string]any{"plan_id": 1}); err != nil {
		t.Fatal(err)
	}
	if len(m.seen) != 2 || m.seen[0].idem == "" || m.seen[0].idem != m.seen[1].idem {
		t.Fatalf("idempotency keys = %q, %q", m.seen[0].idem, m.seen[1].idem)
	}
}

func TestExplicitIdempotencyKey(t *testing.T) {
	m := newMock(t, reply(200, `{}`))
	c, _ := newTestClient(m)
	if _, err := c.Cloud.CreateVolume(context.Background(), map[string]any{}, WithIdempotencyKey("k-1")); err != nil {
		t.Fatal(err)
	}
	if m.seen[0].idem != "k-1" {
		t.Errorf("Idempotency-Key = %q", m.seen[0].idem)
	}
}

func TestConnectionErrorRetriedForReads(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := NewClient("t", WithBaseURL(srv.URL))
	c.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := c.Cloud.ListImages(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d", calls)
	}
}

func TestErrorShapes(t *testing.T) {
	cases := []struct {
		name, body, code, msg, reason string
		status                        int
	}{
		{"core", `{"success":false,"error":{"code":"INSUFFICIENT_BALANCE","message":"Not enough balance"}}`, ErrorCodeInsufficientBalance, "Not enough balance", "", 402},
		{"cloud", `{"error":"Verify your identity","code":"identity_needed","reason":"identity_hold"}`, ErrorCodeIdentityNeeded, "Verify your identity", "identity_hold", 403},
		{"product", `{"code":"not_found","message":"No such server"}`, ErrorCodeNotFound, "No such server", "", 404},
		{"not json", `<html>bad gateway</html>`, "unexpected_response", "request failed with HTTP 400", "", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMock(t, reply(tc.status, tc.body, "X-Request-Id", "req-1"))
			c, _ := newTestClient(m)
			_, err := c.Hosting.ListAccounts(context.Background())
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err = %v", err)
			}
			if apiErr.Code != tc.code || apiErr.Message != tc.msg || apiErr.Reason != tc.reason || apiErr.StatusCode != tc.status || apiErr.RequestID != "req-1" {
				t.Errorf("got %+v", apiErr)
			}
		})
	}
}

func TestSuccessFalseWith200IsAnError(t *testing.T) {
	m := newMock(t, reply(200, `{"success":false,"error":{"code":"NO_DEFAULT_WALLET","message":"No wallet"}}`))
	c, _ := newTestClient(m)
	_, err := c.Core.GetBalance(context.Background())
	if !HasCode(err, ErrorCodeNoDefaultWallet) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegionHeader(t *testing.T) {
	m := newMock(t, reply(200, `{}`), reply(200, `{}`))
	c, _ := newTestClient(m)
	if _, err := c.RegionalVPS.WithRegion("iran-1").ListServers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RegionalVPS.ListServers(context.Background(), WithRegion("iran-2")); err != nil {
		t.Fatal(err)
	}
	if m.seen[0].region != "iran-1" || m.seen[1].region != "iran-2" {
		t.Errorf("regions = %q, %q", m.seen[0].region, m.seen[1].region)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	other := newMock(t)
	m := newMock(t, func(w http.ResponseWriter) {
		w.Header().Set("Location", other.srv.URL+"/steal")
		w.WriteHeader(302)
	})
	c, _ := newTestClient(m)
	_, err := c.Core.GetMe(context.Background())
	if StatusCode(err) != 302 || len(other.seen) != 0 {
		t.Fatalf("err = %v, other saw %d requests", err, len(other.seen))
	}
}

func TestBaseURLMustBeHTTPS(t *testing.T) {
	c := NewClient("t", WithBaseURL("http://api.example.com"))
	if _, err := c.Core.GetMe(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
}

func TestDoForUngeneratedPaths(t *testing.T) {
	m := newMock(t, reply(200, `{"ok":true}`))
	c, _ := newTestClient(m)
	resp, err := c.Do(context.Background(), "GET", "/cloud/v1/something-new", nil, nil)
	if err != nil || resp.StatusCode != 200 || m.seen[0].path != "/cloud/v1/something-new" {
		t.Fatalf("resp = %v, err = %v", resp, err)
	}
}
