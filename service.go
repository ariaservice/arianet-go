package arianet

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// service is embedded in every generated API group.
type service struct {
	client *Client
	region string
}

func (s service) withRegion(region string) service {
	s.region = region
	return s
}

func (s *service) do(ctx context.Context, method, path string, query url.Values, body any, idempotent bool, opts []RequestOption) (*Response, error) {
	return s.client.do(ctx, method, path, query, body, idempotent, s.region, opts)
}

func pathSegment(v any) string {
	return url.PathEscape(fmt.Sprint(v))
}

func queryValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// String returns a pointer to v, for optional parameters.
func String(v string) *string { return &v }

// Int returns a pointer to v, for optional parameters.
func Int(v int64) *int64 { return &v }

// Bool returns a pointer to v, for optional parameters.
func Bool(v bool) *bool { return &v }

// Float returns a pointer to v, for optional parameters.
func Float(v float64) *float64 { return &v }
