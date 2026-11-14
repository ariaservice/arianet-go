# arianet-go

The official Go client for the [Arianet](https://ariaservice.net) public API: Cloud, classic and
regional VPS, hosting, dedicated servers (bare metal), SMS, and your account (API tokens, SSH keys,
firewalls, balance, invoices).

- One method per API operation, generated from the published OpenAPI specs.
- Personal API token auth, retries on `429` that honor `Retry-After`, safe retries of reads and
  idempotent writes after network errors and `502`/`503`/`504`.
- Typed errors carrying the API's stable error codes.
- Standard library only. Go 1.22 or newer.

Documentation: <https://docs.ariaservice.net/sdk/go>

## Install

```bash
go get github.com/ariaservice/arianet-go
```

## Quickstart

Create a personal API token at <https://cloud.ariaservice.net/users/api-tokens> and export it:

```bash
export ARIANET_TOKEN="paste-your-token-here"
```

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ariaservice/arianet-go"
)

func main() {
	ctx := context.Background()
	client := arianet.NewClient("") // empty token: read ARIANET_TOKEN

	resp, err := client.Cloud.ListServers(ctx, &arianet.CloudListServersParams{
		PerPage:       arianet.Int(50),
		LabelSelector: arianet.String("env=prod"),
	})
	if err != nil {
		log.Fatal(err)
	}

	var servers []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := resp.DecodeData(&servers); err != nil {
		log.Fatal(err)
	}
	for _, s := range servers {
		fmt.Println(s.ID, s.Name, s.Status)
	}
}
```

## API groups

| Field                | API                                                               | Path prefix     |
|----------------------|-------------------------------------------------------------------|-----------------|
| `client.Core`        | Account, API tokens, classic VPS, SSH keys, firewalls, billing    | `/api/v1`       |
| `client.Cloud`       | Cloud servers, volumes, networks, firewalls, IPs, load balancers  | `/cloud/v1`     |
| `client.RegionalVPS` | Regional VPS                                                      | `/vps/v1`       |
| `client.Hosting`     | Hosting accounts, DNS, mailboxes, databases                       | `/hosting/v1`   |
| `client.Baremetal`   | Dedicated servers                                                 | `/baremetal/v1` |
| `client.SMS`         | SMS messages, templates, lines, webhooks                          | `/sms/v1`       |

Method names follow the `operationId` of each operation in the API reference
(<https://docs.ariaservice.net/api-reference>): `listServers` becomes `ListServers`. Path
parameters are arguments, a JSON request body is `body any` (a struct, a map or `json.RawMessage`),
and query parameters are an optional `*<Group><Method>Params` struct whose fields are pointers
(use `arianet.String`, `arianet.Int`, `arianet.Bool`).

Every method returns `*arianet.Response` with `StatusCode`, `Header` and the raw `Body`. Use
`Decode(&v)` for the whole body or `DecodeData(&v)` for its `data` member.

```go
resp, err := client.Cloud.CreateServer(ctx, map[string]any{
	"name":       "web-1",
	"plan_id":    101,            // from client.Cloud.ListPlans
	"image":      "ubuntu-24.04", // an image slug from client.Cloud.ListImages
	"datacenter": "ovh",          // a datacenter code from client.Cloud.ListDatacenters
})
```

## Configuration

```go
client := arianet.NewClient(token,
	arianet.WithBaseURL("https://api.ariaservice.net"), // default, or $ARIANET_API_URL
	arianet.WithMaxRetries(3),                           // default 3
	arianet.WithMaxRetryWait(60*time.Second),            // longest wait before one retry
	arianet.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}),
	arianet.WithUserAgent("my-app/1.2"),
)
```

The base URL must use `https` (plain `http` is accepted for `localhost` only). The client never
follows redirects, so the token is only ever sent to the API host.

## Retries and rate limits

- `429 Too Many Requests` is retried for every method, after the `Retry-After` header (or
  `RateLimit-Reset`). If the server asks for a longer wait than `WithMaxRetryWait`, the error is
  returned at once with `APIError.RetryAfter` set.
- Network errors and `502`/`503`/`504` are retried only when repeating cannot do the work twice:
  `GET`/`HEAD` requests and requests with an `Idempotency-Key`.
- Operations that accept an `Idempotency-Key` (creating a server, an order, a hosting account...)
  get a generated key automatically, so they are retried safely. Pass your own with
  `arianet.WithIdempotencyKey(key)` to make retries across process restarts safe too.

## Errors

```go
_, err := client.Core.CreateServer(ctx, order)
var apiErr *arianet.APIError
switch {
case arianet.HasCode(err, arianet.ErrorCodeInsufficientBalance):
	// top up the wallet
case errors.As(err, &apiErr):
	log.Printf("HTTP %d %s: %s (request %s)", apiErr.StatusCode, apiErr.Code, apiErr.Message, apiErr.RequestID)
case err != nil:
	log.Fatal(err) // *arianet.ConnectionError or a context error
}
```

`APIError.Code` is the API's stable machine code, always lower-cased (the core API's
`INSUFFICIENT_BALANCE` becomes `insufficient_balance`), so it can be compared with the generated
`arianet.ErrorCode...` constants. Branch on the code, not on `Message`. `Reason` narrows the code
on some refusals (for example `identity_hold`). Helpers: `arianet.IsNotFound`,
`arianet.IsUnauthorized`, `arianet.IsForbidden`, `arianet.IsRateLimited`, `arianet.StatusCode`.

## Regions

Product APIs with regional installs take the `Arianet-Region` header:

```go
iran := client.RegionalVPS.WithRegion("iran-1")
resp, err := iran.ListServers(ctx)
// or per request:
resp, err = client.RegionalVPS.ListServers(ctx, arianet.WithRegion("iran-1"))
```

## Endpoints not in this version

`client.Do(ctx, method, path, query, body, opts...)` calls any path with the same auth, retries
and error handling:

```go
resp, err := client.Do(ctx, "GET", "/cloud/v1/new-endpoint", url.Values{"page": {"2"}}, nil)
```

## How the code is generated

`specs/` holds the published OpenAPI documents and `codegen/services.json` lists where they come
from. `*_gen.go` is produced by `codegen/generate.py` (Python standard library plus `gofmt`):

```bash
python3 codegen/fetch_specs.py   # refresh specs/ (needs: pip install pyyaml)
python3 codegen/generate.py      # rewrite *_gen.go
python3 codegen/generate.py --check
```

The *Regenerate from specs* workflow does this every day and opens a pull request when the
published specs change. The hand-written runtime (`client.go`, `errors.go`, `response.go`,
`service.go`) is never touched by the generator.

## License

MIT, see [LICENSE](LICENSE). Report security problems privately, see [SECURITY.md](SECURITY.md).
