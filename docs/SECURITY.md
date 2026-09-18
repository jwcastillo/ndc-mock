# Security and practices

Audit of this project, with what was found and what was done about it. Re-run the checks below
after changes.

## Threat model

This is a **test double**. It holds no real customer data, authenticates nobody, and is meant to be
reachable by a load generator. That shapes what matters:

- It is a **target**, so resource exhaustion is the primary risk — a load tool sends whatever the
  operator types, and a single header should not be able to take the process down.
- In **proxy mode** it handles someone else's credentials in transit.
- It is **published under Apache-2.0**, so provider data and client identity must not leak into the
  repository.

Out of scope: authentication and authorization. The mock deliberately accepts any credentials; that
is the point of running without secrets. **Do not expose it to an untrusted network.**

## Findings and resolutions

### Resource exhaustion via `X-Mock-Offers` — fixed

A single request with `X-Mock-Offers: 1000000` drove the process to **1.4 GB RSS** and never
completed. Each offer is tens of kilobytes and the whole document is assembled in memory before it
is written, so one header amplified into gigabytes.

Bounded by `-max-offers` (default 5000). The same request now serves 5000 offers, peaks at 262 MB
and answers in 0.14 s.

### Connection pinning via `X-Mock-Delay` — fixed

Simulated latency was unbounded, so `X-Mock-Delay: 1h` held a connection for an hour. Bounded by
`-max-delay` (default 2m, high enough for a slow order operation's tail; it bounds configured latency and the header alike).

### No server timeouts — fixed

The server used `http.ListenAndServe`, whose zero-value `http.Server` has no timeouts at all,
leaving a slow or stalled client holding a connection indefinitely. Now set explicitly:

| | |
|---|---|
| `ReadHeaderTimeout` | 10s |
| `ReadTimeout` | 60s |
| `WriteTimeout` | 5m (a large response over a slow link) |
| `IdleTimeout` | 120s |
| `MaxHeaderBytes` | 1 MiB |

### Buffer pool retaining oversized buffers — fixed

Response buffers were returned to the pool regardless of size, so one very large response pinned its
capacity for the life of the process. Buffers above 32 MiB are now dropped rather than recycled.

### Client identity in the repository — fixed

Real agency identifiers, an IATA number, an account code and a contact email were present in
`dataset/` and `capture/`. Replaced with example values. A plaintext credentials file created during
development was deleted and the pattern gitignored.

### Compiled binaries would have been committed — fixed

`bin/` held 16 MB of binaries outside `.gitignore`.

## Verified as safe

**XML external entities.** Go's `encoding/xml` does not resolve DTDs or expand entities, so neither
XXE nor entity-expansion applies. Confirmed by sending a document with an external entity and a
nested-entity payload: no file contents came back, no expansion occurred.

```
{"code":400,"message":"ndc-mock: request carries no OrderID","owner":"NDC_MOCK"}
```

**Request body size.** Every body read goes through `io.LimitReader` — 4 MiB for shopping, 8 MiB for
the ordering operations and the proxy path.

**Concurrency.** The order store is mutex-guarded. Built with `-race` and driven with 40 concurrent
`OrderCreate` calls interleaved with reads: no data races, all 40 orders stored.

**Unbounded order growth.** The store expires entries by TTL and evicts the oldest past
`-order-max`, so a long run cannot grow without limit.

**Input validation in the client.** Airport codes, dates, carrier codes and passenger counts are
checked against their expected shape before a request document is assembled, and identifiers are
rejected if they carry markup characters. Values that do reach a document are escaped with
`xml.EscapeText`.

**Supply chain.** Zero third-party dependencies in either module — no `go.sum`, stdlib only. The MCP
server speaks JSON-RPC directly rather than taking on an SDK that would need tracking as the spec
moves.

**Credential handling.** Credentials are read from the environment, never from the command line
where the process table and shell history would retain them. In proxy mode the caller's headers are
forwarded verbatim and never read, logged or stored. The capture script writes responses to disk but
never credentials.

## Residual risks

- **No authentication.** By design. Bind it to a trusted network; do not put it on the public
  internet.
- **A 5000-offer response is still 67 MB.** The ceiling is generous because a load test needs large
  payloads on purpose. Lower `-max-offers` if the mock is reachable by anything you do not control.
- **Order store eviction is O(n)** when full — it scans for the oldest entry on insert. At the
  default 50000 that is measurable under sustained order creation. Fine for the intended use; if it
  matters, replace with a ring or a heap.
- **Proxy mode trusts its configured upstream.** The URL comes from the operator, not from a
  request, so this is not request-driven SSRF, but a misconfigured upstream receives forwarded
  credentials.
- **No panic recovery middleware.** `net/http` isolates a panic to its own connection, so a bad
  request cannot take the process down, but there is no structured reporting.

## Re-running the audit

```bash
# secrets and client identity
grep -rniE '(password|secret|token|api[_-]?key)[[:space:]]*[:=][[:space:]]*["'"'"'][^"'"'"']{8,}' \
  --include='*.go' --include='*.json' --include='*.sh' . | grep -v '^./local/'

# resource ceilings
curl -s -o /dev/null -D - -X POST localhost:8090/ndc/v192/airshopping \
  -H 'X-Mock-Offers: 1000000' --data-binary @dataset/gru-nat-64.xml | grep -i x-mock-offers

# races
cd edge && go build -race -o /tmp/edge-race . && /tmp/edge-race -stubs ... &
# drive concurrent order creation, then grep the log for DATA RACE

# dependencies
cd edge && go list -m all      # expect one module
cd client && go list -m all    # expect one module
```
