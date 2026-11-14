package arianet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// APIError is an error answer from the API.
type APIError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Code is the API's stable machine code, lower-cased (compare it with the
	// ErrorCode... constants). Branch on Code, not on Message.
	Code string
	// Message is human-readable and may change.
	Message string
	// Reason narrows Code on some refusals (e.g. "identity_hold").
	Reason string
	// RetryAfter is how long the server asked to wait (Retry-After), if it did.
	RetryAfter time.Duration
	// RequestID identifies the request in support tickets, when the server sent one.
	RequestID string
	// Body is the raw response body.
	Body []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("arianet: %s (HTTP %d, code %s)", e.Message, e.StatusCode, e.Code)
}

// ConnectionError means the request got no answer (DNS, TLS, timeout, reset).
type ConnectionError struct{ Err error }

func (e *ConnectionError) Error() string { return "arianet: request failed: " + e.Err.Error() }
func (e *ConnectionError) Unwrap() error { return e.Err }

// HasCode reports whether err is an *APIError with the given code.
func HasCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == strings.ToLower(code)
}

// StatusCode returns the HTTP status of an *APIError, or 0.
func StatusCode(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// IsNotFound reports a 404 answer.
func IsNotFound(err error) bool { return StatusCode(err) == http.StatusNotFound }

// IsRateLimited reports a 429 answer that was still refused after the retries.
func IsRateLimited(err error) bool { return StatusCode(err) == http.StatusTooManyRequests }

// IsUnauthorized reports a 401 answer (missing, expired or revoked token).
func IsUnauthorized(err error) bool { return StatusCode(err) == http.StatusUnauthorized }

// IsForbidden reports a 403 answer (missing scope, IP not allowed, identity check needed...).
func IsForbidden(err error) bool { return StatusCode(err) == http.StatusForbidden }

// errorFromResponse returns the *APIError a response stands for, or nil.
// It reads the three error shapes the APIs use:
//
//	{"success": false, "error": {"code": "...", "message": "..."}}  core API
//	{"error": "message", "code": "...", "reason": "..."}            cloud API
//	{"code": "...", "message": "..."}                               product APIs
func errorFromResponse(r *Response) *APIError {
	var probe struct {
		Success *bool           `json:"success"`
		Error   json.RawMessage `json:"error"`
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Reason  string          `json:"reason"`
	}
	decoded := json.Unmarshal(r.Body, &probe) == nil
	failed := r.StatusCode >= 300 || (decoded && probe.Success != nil && !*probe.Success)
	if !failed {
		return nil
	}
	e := &APIError{
		StatusCode: r.StatusCode,
		RetryAfter: parseRetryAfter(r.Header, r.StatusCode),
		RequestID:  firstHeader(r.Header, "X-Request-Id", "X-Request-ID", "Request-Id"),
		Body:       r.Body,
	}
	if decoded {
		e.Code, e.Message, e.Reason = probe.Code, probe.Message, probe.Reason
		var text string
		var nested struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
		}
		switch {
		case len(probe.Error) == 0:
		case json.Unmarshal(probe.Error, &text) == nil:
			e.Message = text
		case json.Unmarshal(probe.Error, &nested) == nil:
			if nested.Code != "" {
				e.Code = nested.Code
			}
			if nested.Message != "" {
				e.Message = nested.Message
			}
			if nested.Reason != "" {
				e.Reason = nested.Reason
			}
		}
	}
	if e.Code == "" {
		e.Code = defaultCode(r.StatusCode, decoded)
	}
	e.Code = strings.ToLower(e.Code)
	if e.Message == "" {
		e.Message = fmt.Sprintf("request failed with HTTP %d", r.StatusCode)
	}
	return e
}

func defaultCode(status int, decoded bool) string {
	if !decoded {
		return "unexpected_response"
	}
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	return "request_failed"
}

func firstHeader(h http.Header, names ...string) string {
	for _, n := range names {
		if v := h.Get(n); v != "" {
			return v
		}
	}
	return ""
}
