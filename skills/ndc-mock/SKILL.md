---
name: ndc-mock
description: Drive and operate ndc-mock, a synthetic IATA NDC mock for load testing, and its two clients - the `ndc-mcp` MCP server (tools ndc_search, ndc_price, ndc_order_create, ndc_order_retrieve, ndc_order_cancel, ndc_services, ndc_seats) and the `ndc` CLI. Use when shopping, pricing or booking through the ndc_* MCP tools, running the `ndc` CLI, starting or configuring the mock (versions, X-Mock-* headers, routes/airlines config, multi-city, proxy mode, replicas), or sizing NDC payloads for a load test.
---

# ndc-mock

A synthetic IATA NDC endpoint: the full shopping and ordering flow, no airline API at runtime.
Full reference: `README.md`, `docs/ARCHITECTURE.md`, `docs/SECURITY.md` in this repository.

## MCP tools (`bin/ndc-mcp`)

| Tool | Required | Optional | Notes |
|---|---|---|---|
| `ndc_search` | `origin`, `destination`, `departureDate` | `returnDate`, `adults` (1), `children`, `infants`, `airline`, `maxOffers` (10) | One way or return; no multi-city |
| `ndc_price` | `offerId` | | Firm price |
| `ndc_order_create` | `offerId` | | **Books. Changes state - confirm with the user first** |
| `ndc_order_retrieve` | `orderId` | | Order and status |
| `ndc_order_cancel` | `orderId` | | **Irreversible - confirm with the user first** |
| `ndc_services` | `offerId` | `count` | Ancillaries |
| `ndc_seats` | `offerId` | `rows` | Seat map |

How to use them well:

- **Chain by identifier.** `offerId` from `ndc_search` feeds price, order, services and seats;
  `orderId` from `ndc_order_create` feeds retrieve and cancel. Pass them through verbatim - they are
  self-describing (route, dates, carrier, fare, passengers), which is what lets the flow work
  without session state. Never edit or construct one.
- **Keep `maxOffers` small.** Every offer is tokens. Raise it only when the user asks for more.
- **Validate before calling.** Airports are three letters, dates `YYYY-MM-DD`, return not before
  departure, infants not more than adults. The tools reject these anyway; checking first saves a turn.
- **Errors come back as tool content with `isError`**, not protocol errors. Read the text, fix the
  input, retry once.
- **Price before booking.** Search -> price -> confirm with the user -> order.

Registration:

```json
{ "mcpServers": { "ndc": { "command": "/path/to/bin/ndc-mcp", "args": ["-url", "http://localhost:8090"] } } }
```

Flags: `-url` / `$NDC_URL` (default `http://localhost:8090`), `-version` / `$NDC_VERSION` (default
`v192`), `-timeout` (60s). Credentials only from the environment: `$NDC_AUTH` (sent as
`Authorization`), `$NDC_API_KEY` (sent as `X-Api-Key`) - never in args or config files.

Both clients ask for the mock's **JSON projection** (`Accept: application/json`), which the mock
builds from the identifiers. A real provider answers NDC XML, so point the clients at the mock.

## CLI (`bin/ndc`)

```bash
ndc search -from GRU -to NAT -depart 2026-09-30 [-return 2026-10-02] [-adults 1 -children 0 -infants 0] [-airline XX] [-max 10]
ndc price <offer-id>
ndc order <offer-id>
ndc retrieve <order-id>
ndc cancel <order-id>
ndc services <offer-id>
ndc seats <offer-id>
```

`-json` prints the raw projection (before or after the verb). Same `-url`, `-version`,
`-timeout` and credential variables as the MCP server. `make build` produces both binaries.

## Running the mock

```bash
make edge        # :8090, stubs from ./stubs
make test        # unit + flow + translation + proxy + MCP + replicas + Bruno
make image       # distroless image; mount references at /stubs
```

- **Only AirShopping needs a reference response** (`stubs/<version>/`, gitignored - it is provider
  data). Everything after it is generated from the offer identifier. Orders live in memory
  (`-order-ttl` 1h, `-order-max` 50000).
- **Operations** under `/ndc/{v}/`: `airshopping`, `offerprice`, `ordercreate`, `orderretrieve`,
  `orderchange`, `ordercancel`, `orderreshop`, `servicelist`, `seatavailability`. Plus
  `/oauth/token` (any credentials), `/__stats`, `/__health`. `-operations <json>` maps a provider's
  own path layout onto these handlers.
- **Versions** by URL segment: `v192`, `v213`, `v241`, `v244` (`versions.json`). A version without
  its own reference is translated at startup from one that has (`translations/`), and validates
  against IATA's XSD. Neither reference nor mapping -> `501` naming what is missing. What IATA
  published as 21.3.6 is 24.1.
- **Mappings are derived, never hand-written**: `tools/derive-mapping.py`, checked on paths with
  `tools/compare-paths.py`, final word `tools/validate-translation.sh <version> <xsd-dir>`. The XSDs
  are licensed by IATA and are not in the repository.

### Per-request dials (for sweeping in a load test without restarts)

| Header | Effect |
|---|---|
| `X-Mock-Offers` | Offers, ancillaries or seat rows (capped by `-max-offers`, 5000) |
| `X-Mock-Delay` | `900ms` or a range `900ms~2s` (capped by `-max-delay`, 2m) |
| `X-Mock-Airline` | Carrier profile from `airlines.json` |
| `Accept: application/json` / `?format=json` | JSON projection instead of XML |
| `Accept-Language` | Language (`-lang-header` for a vendor header) |

Responses echo `X-Mock-Offers`, `X-Mock-Airline`, `X-Mock-Version`, and `X-Mock-Translated` plus
`X-Mock-Translation-Coverage` when translated.

Payload size and offer count, not request rate, drive NDC cost - sweep those. **Latency is zero
until configured**: put the provider's real p50/p95 in `routes.json` before trusting a benchmark.

### Multi-city

Matched by itinerary **shape** (airports numbered by first appearance): `0-1,1-2,2-0` circuit,
`0-1,1-2,2-3` open chain, `0-1,2-0` open jaw. One `air-shopping-mc*.xml` reference per shape; a
shape with none answers `501` listing the loaded shapes.

### Proxy mode

`-proxy <url>` forwards to a real provider, falling back to synthetic if it is unreachable;
`-record <dir>` keeps the responses; `-forward-headers` lists what goes upstream.
`tools/probe-proxy.sh` exercises it with read-only calls only (never OrderCreate).

### Several replicas

Set `NDC_MOCK_ADVERTISE` (this pod's `ip:port`) and `NDC_MOCK_PEERS` (a headless Service name).
Order ids then carry their owner, and other replicas forward to it - only to addresses `-peers`
resolves, so a crafted id cannot turn the mock into an open proxy. A restarted replica loses its
orders (`502`).

## Safety

No authentication by design. Do not expose it to an untrusted network.
