# ndc-mock

A synthetic IATA NDC mock for load testing. It implements the full shopping and ordering flow,
answers in NDC XML **or** a BFF-style JSON projection, simulates any carrier you configure, and
scales payload size on demand — without calling an airline API at runtime.

Built for capacity work. The cost of an NDC response is driven by **payload size and offer count**,
not by request rate, so those are the dials this exposes.

```bash
make edge &        # starts on :8090
make test          # smoke tests + Bruno collection
```

---

## Table of contents

- [What it implements](#what-it-implements)
- [Two wire formats](#two-wire-formats)
- [Carrier simulation](#carrier-simulation)
- [Sizing and latency](#sizing-and-latency)
- [Version support](#version-support)
- [Version translation](#version-translation)
- [Per-request overrides](#per-request-overrides)
- [Configuration files](#configuration-files)
- [How it works](#how-it-works)
- [Performance](#performance)
- [CLI and MCP](#cli-and-mcp)
- [Running it](#running-it)
- [Testing](#testing)
- [Multi-city](#multi-city)
- [Getting a reference response](#getting-a-reference-response)
- [What it does not do](#what-it-does-not-do)
- [License](#license)

---

## What it implements

The whole flow, stateful from `OrderCreate` onwards.

| Operation | Path | Response | Notes |
|---|---|---|---|
|  |  |  | ✓ = validates against IATA's XSD in 19.2, 21.3, 24.1 and 24.4 |
| Token | `/oauth/token` (`-token-path`) | JSON | Accepts any credentials by design |
| AirShopping | `/ndc/{v}/airshopping` | `IATA_AirShoppingRS` | ✓ From a reference response; offer count configurable; one way, return and multi-city |
| OfferPrice | `/ndc/{v}/offerprice` | `IATA_OfferPriceRS` | ✓ Prices the offer it is handed |
| OrderCreate | `/ndc/{v}/ordercreate` | `IATA_OrderViewRS` | ✓ Mints an order, returns `201` |
| OrderRetrieve | `/ndc/{v}/orderretrieve` | `IATA_OrderViewRS` | ✓ `404` on an unknown order |
| OrderChange | `/ndc/{v}/orderchange` | `IATA_OrderViewRS` | ✓ |
| OrderCancel | `/ndc/{v}/ordercancel` | `IATA_OrderCancelRS` (19.2), `IATA_OrderViewRS` (21.3+) | ✓ Status `CLOSED`. IATA dropped OrderCancel after 19.2 |
| OrderReshop | `/ndc/{v}/orderreshop` | `IATA_OrderReshopRS` | ✓ Alternatives at a spread of fares |
| ServiceList | `/ndc/{v}/servicelist` | `IATA_ServiceListRS` | ✓ Ancillary catalogue as a la carte items with their service definitions, count configurable |
| SeatAvailability | `/ndc/{v}/seatavailability` | `IATA_SeatAvailabilityRS` | ✓ Seat map, row count configurable; seats priced through a la carte items by row tier, status in IATA codes (`F` free, `O` occupied) |
| Stats | `/__stats` | JSON | Orders held, versions, templates |
| Health | `/__health` | text | |

**Providers lay their API out differently.** The default is one path per IATA message. `-operations`
takes a JSON file mapping each path to one of the handlers (`airshopping`, `offerprice`,
`ordercreate`, `orderretrieve`, `orderchange`, `ordercancel`, `orderreshop`, `servicelist`,
`seatavailability`), so a provider that serves several paths per message, or nests them, can be
mirrored exactly:

```json
{ "routes": { "shop": "airshopping", "order/create": "ordercreate", "order/create/instant": "ordercreate" } }
```

One handler outside the IATA messages is available for such layouts: `installments`, payment plans
answered as `IATA_OrderRulesRS`, which no IATA schema defines.

Only AirShopping needs a reference response. **Everything after it is generated** — the offer
identifier is self-describing, carrying route, dates, carrier, flight, fare and passenger mix, so
the trip can be rebuilt from it and stays coherent through the flow.

### State

`OrderCreate` mints a record-locator-shaped id and stores the order in memory. Retrieve, change and
cancel operate on it and the transitions persist:

```
Created -> Changed -> Cancelled
```

The store is a cache, not a database: entries expire (`-order-ttl`, default 1h) and the oldest is
evicted past `-order-max` (default 50000). A load test never revisits an old order.

---

## Two wire formats

Every operation answers in NDC XML or a reduced JSON projection, the way a BFF in front of an NDC
provider does. Select with `Accept: application/json` or `?format=json`.

```bash
curl "$B/airshopping"              --data-binary @rq.xml   # IATA_AirShoppingRS
curl "$B/airshopping?format=json"  --data-binary @rq.xml   # BFF projection
```

```json
{
  "shoppingResponseId": "SHOP-GRUNAT-2026-09-30",
  "origin": "GRU", "destination": "NAT",
  "currency": "USD", "offerCount": 64,
  "offers": [{
    "offerId": "<the provider's identifier, re-encoded for this trip>",
    "price": { "currency": "USD", "total": 812, "base": 666, "taxes": 146 },
    "segments": [{ "origin": "GRU", "destination": "NAT", "carrier": "XX",
                   "flightNumber": "1234", "departure": "...", "arrival": "..." }],
    "passengers": { "adt": 1, "chd": 0, "inf": 0 },
    "roundTrip": true
  }]
}
```

The JSON is built from the decoded identifiers, **not** by converting the XML. A BFF returns far
less than NDC does, and parsing several megabytes per request would dominate the response time.
Errors come back in whichever format was asked for.

The `offerId` is the **real, self-describing identifier**, not a synthetic label. That is what makes
the chain work: hand it back to `offerprice` or `ordercreate` and the trip, carrier and fare come
through intact, with no server-side session state.

---

## Carrier simulation

The same itinerary, presented as different airlines. Carrier code, currency and fare magnitude come
from the profile; the **fare distribution of the reference response is preserved**, because real
pricing produces a spread across brands and cabins that is hard to fake convincingly.

| `X-Mock-Airline` | Identifier | Fare |
|---|---|---|
| *(none)* | `IF=NAT,GRU,1234,XX` | `PR=BRL/4390` |
| `IB` | `IF=NAT,GRU,1234,IB` | `PR=EUR/817` |
| `AV` | `IF=NAT,GRU,1234,AV` | `PR=COP/2540422` |
| `AA` | `IF=NAT,GRU,1234,AA` | `PR=USD/812` |

The carrier applies to the ordering half of the flow too, so an order priced as `IB` comes back in
EUR throughout.

---

## Sizing and latency

Offer count drives payload size, which is what a load test is usually probing:

| Offers | Payload |
|---|---|
| 64 | 1.8 MB |
| 256 | 4.2 MB |
| 484 | 7.1 MB |

Offer counts on a round trip tend toward perfect squares (8², 16², 22²) because each outbound
itinerary combines with each inbound one, so offers grow quadratically with route density. On a real
provider this is driven by `ConnectionCriteria/MaximumConnectionQty` — allowing one connection
instead of none took one route from 64 offers to 748.

`X-Mock-Offers` also sizes the ancillary catalogue and the seat map (rows × 6 columns).

**Latency ships at zero, uncalibrated, on purpose.** An instant mock measures your service in a
vacuum. Put your provider's real p50/p95 into `routes.json` before trusting a benchmark.

A delay is either fixed (`"850ms"`) or a distribution (`"850ms~2.4s"`, meaning p50~p95). The
distribution is a log-normal through both points. Use it: in-flight requests scale with the
**mean**, not the median, and a fixed delay at the median understates how many connections your
service holds open against the real provider.

Shopping latency is set per route. Everything after it is set per operation, because those calls
carry an offer, not a search, and their cost does not follow the route:

```json
"operations": { "offerprice": "400ms~1.1s", "ordercreate": "1.2s~3s" }
```

Numbers measured from a real provider's production belong to that provider, so keep a calibrated
profile outside the repository: `local/` is ignored, and `make edge CONFIG=local/routes.prod.json`
runs with it. A provider's own per-request log is the best source: a gateway that keeps only the
sum and count of its latency histograms gives you the mean and no percentiles.

---

## Version support

Routed by URL segment. The engine works on text markers rather than a schema, so it is
version-agnostic — a profile declares only the element names the slicer needs.

```
version v192 (IATA 19.2) ready
version v213 (IATA 21.3): no reference response under stubs/21.3
version v241 (IATA 24.1): no reference response under stubs/24.1
version v244 (IATA 24.4): no reference response under stubs/24.4
```

Adding a generation is dropping a reference response into `stubs/<version>/` and declaring its
element names in `versions.json`. A version with neither a reference nor a usable mapping **answers
501 naming what is missing**, rather than quietly serving another generation's payload.

IATA renamed a release: what was published as **21.3.6 is 24.1**. Anything labelled 21.3.6 describes
24.1.

**Requests are read in either generation's shape.** 21.3 wrapped the AirShopping criteria in
`FlightRequest/FlightRequestOriginDestinationsCriteria` and renamed `Paxs` to `PaxList`. The IATA
requests of every generation point at offers and orders by reference (`OfferRefID`,
`OfferItemRefID`, `OrderRefID`), while provider dialects often use `OfferID` and `OrderID` directly.
The mock takes both, and the direct name wins when a request carries both. From 21.3 a cancel arrives as an
`OrderChangeRQ`. AirShopping by specific origin-destination or by affinity answers `400` naming what
is supported.

---

## Version translation

A generation with no reference response of its own is built by translating one that has, **once,
at startup**. The result is an ordinary template of the target generation, so serving it costs the
same as serving a native one (20 ms against 26 ms for 484 offers, 6.9 MB against 7.1 MB):

```
v241|rt   BOG-MCO 2026-05-15 RT 2026-06-15  ...  196 offers, 2.9 MB  translated 19.2->24.1
version v241 (IATA 24.1) ready, translated from 19.2
```

```
$ curl -D - -X POST localhost:8090/ndc/v241/airshopping --data-binary @rq.xml
X-Mock-Translated: 19.2->24.1

<m:IATA_AirShoppingRS xmlns="http://www.iata.org/IATA/2015/EASD/00/IATA_OffersAndOrdersCommonTypes"
                      xmlns:m="http://www.iata.org/IATA/2015/EASD/00/IATA_OffersAndOrdersMessage">
  <m:Response>
    <DataLists>...
```

**The translated documents validate against IATA's official XSD** for 21.3, 24.1 and 24.4. That
holds for every reference (round trip, one way, en/es/pt), for the three dataset routes, and with
offers recycled up to 500:

```bash
tools/validate-translation.sh v241 /path/to/24_1_distribution_schemas
```

```
round-trip: valid (64 offers)
round-trip-500: valid (500 offers)
one-way: valid (64 offers)
...
```

### What the translation does

From 21.3 a message is no longer in a namespace of its own. The root and its direct children
(`Response`, `Error`, `PayloadAttributes`, `AugmentationPoint`) are in the shared
`IATA_OffersAndOrdersMessage` namespace, and everything below them is in
`IATA_OffersAndOrdersCommonTypes`. The rest is structure, not names:

| Change | How it is translated |
|---|---|
| `PaxSegment` held the whole flight; from 21.3 it points at a `DatedMarketingSegment`, which points at a `DatedOperatingSegment` made of `DatedOperatingLeg`s, each in its own list | Split one to one, with ids derived from the `PaxSegmentID` (`DMS_`, `DOS_`, `LEG_`) |
| Every sequence is in alphabetical order, ignoring case (409 of 409 in 21.3, 426 of 426 in 24.1 and 24.4) | Children are sorted, with no per-type data |
| `TimeZoneCode` attribute removed from times | Folded into the `xs:dateTime` value (`13:55:00-04:00`), lossless |
| Weight unit moved off each measure to `WeightUnitOfMeasurement`, in UN/ECE codes | `KG` to `KGM`, `POUNDS` to `LBR` |
| `PaxSegmentRefID` and `PaxJourneyRefID` now sit inside wrappers | Wrapped |
| `FareRule/PenaltyRefID` moved up to `FareDetail` | Hoisted, duplicates removed |
| `Penalty` requires a `Price`; the provider sends penalties with no amount | Removed with their references. Inventing an amount would misstate the fare; what they said survives in each offer item's change and cancel restrictions |
| `FarePriceTypeCode` became a closed list (`Filed`, `Net`) | The provider's `SELL_AMOUNT` is a published selling fare, so `Filed` |
| Four elements renamed, `Offer/BaggageAllowance` renamed only under `Offer`, fifteen removed | By mapping, each rule checked against all three schemas |

The rules live in `translations/19.2-to-<version>.json`, with the evidence for each in its `notes`.
Transforms that build new lists and references are named there and implemented in
`edge/translate.go`.

**The flow after shopping** is built by the mock in 19.2 shape, valid against the 19.2 schema, and
served in later generations through the same mapping and the same engine, per request (the
documents are a few kilobytes). Three changes are specific to those messages:

- An order's service association becomes `OrderServiceAssociation`, and an offer's becomes
  `OfferServiceAssociation`. Renames are keyed by path suffix
  (`OrderItem/Service/ServiceAssociations`) for exactly this.
- A reshop names the order it reshops from 21.3 on. 19.2 has no place for it, so the handler adds
  it only when the target is 21.3 or later.
- IATA removed OrderCancel after 19.2: an order is cancelled through OrderChange and answered with
  OrderView. A cancel under `/ndc/v213/` and later answers `IATA_OrderViewRS` with status `CLOSED`.

`validate-translation.sh` walks that flow for a single adult on a round trip and for a family on
one way under another carrier profile, in every generation. It sends schema-valid requests in that
generation's shape, from `dataset/rq/19.2/` and `dataset/rq/21.3/` (21.3 introduced the request
shape that 24.1 and 24.4 keep, except that 24.1 renamed `SeatAvailibilityCoreRequest` to
`SeatAvailCoreRequest`, which `dataset/rq/24.1/` overrides). It validates each request before sending it. That way a failure is
the mock's, not the fixture's.

ServiceList and SeatAvailability add three more. An a la carte `ALaCarteOfferItem` becomes `OfferItem`
and its `FlightAssociations` becomes `OfferFlightAssociations`, with the same segment wrapper. Each
seat-map column is wrapped in its own `SeatColumn` (`wrapEach`). Each seat also repeats its row's
`RowNumber`, which 21.3 requires.

**Scope.** Elements that neither the references nor the builders produce are untested, even where a
rule would apply to them.

### How a mapping is derived

Never by hand. Names come from `tools/derive-mapping.py`. Each rule then has to hold up on **paths**,
because a name alone cannot tell a rename from a move:

```bash
tools/compare-paths.py 19.2/IATA_AirShoppingRS.xsd 24_1_distribution_schemas/IATA_AirShoppingRS.xsd
```

For every name that the later generation lacks, the tool prints each place it was used, whether
that parent still exists, and what is new under it. `--typesafe` annotates each of those with a
reading — `renamed to X 0.87`, `restructured: wrapped in Y`, and what came second when it was
close. Wraps are settled in code, from the paths alone: the old name reappearing inside a candidate,
or a candidate with one child carrying the old subtree, both prove a new level rather than a new
name. Only what that cannot decide is asked.

Nothing is auto-accepted and there is no threshold. Measured on the real schemas — 19.2
`IATA_AirShoppingRS.xsd` against the 21.3 and the 24.1 distributions, 87 and 86 unresolved paths,
about 3 seconds and one request each — the readings agree with the shipped mapping on **19 of the
22 paths that mapping covers**, the same score on both pairs. Every rename is right: 4 of 4 per
pair, at 0.71 to 0.97, and no path the mapping drops was ever read as a rename.

The three disagreements are the same failure both times, and they are the reason there is no
threshold. `DataLists/FareList`, `DataLists/MediaList` and `Offer/OwnerTypeCode` are drops, and all
three were read as restructures at 0.81, 0.62 and 0.41 — the parent gained unrelated children and
that was enough. Nothing in the paths separates them from a rename, either: `CharacteristicCode`
also vanishes from 21.3 completely and *is* renamed. Trust a rename reading; check a restructure
yourself. It follows `xs:import`, which matters because
from 21.3 the types live in `IATA_OffersAndOrdersCommonTypes.xsd`. It also reads IATA's SVG
diagrams, and on 24.1 it agrees with the XSD on 2,046 of 2,054 paths (the gap is digital-signature
elements). `validate-translation.sh` then has the final word.

What string similarity cannot settle it dumps in a list for someone to read. `--typesafe` sorts
that list instead, asking a [TypeSafe](https://typesafe.ai) Choice per leftover name — the closest
target names plus *none of these* — and routing each answer by its own confidence into a proposed
rename, a proposed drop, or too uncertain to say:

```bash
export TYPESAFE_API_KEY=...          # from a gitignored *.env, like the provider credentials
tools/derive-mapping.py 19.2/ 25_1/ --from-version 19.2 --to-version 25.1 --typesafe
```

Only element **names** leave the machine, never a captured response, and the key is read from the
environment alone: it is not a flag, and it is not written to the mapping.

The proposals land in a `review` key, never in `rename`. Reading order is all they are: a rename
still has to hold up on paths and then validate, because a wrong one produces a document that looks
converted and is not.

The default threshold of 0.8 comes from one case worth keeping: similarity pairs 19.2 `RepriceOrder`
with 21.3 `ServiceOrder` at 0.83 and puts it first, but 21.3 replaced that empty element with
`ReshopOrder/ReshopOrderChoice/ServiceOrder` — a structural change, which is not a rename in either
direction. The model answers at 0.49, under the threshold, so the name goes to a person instead of
to the top of the list. Raise or lower 0.8 on your own schema pair; it is not a constant worth
trusting unmeasured.

The IATA schemas are licensed and are **not** in this repository. Download them from IATA.

---

## Per-request overrides

So a load test can sweep values without editing config or restarting.

| Header | Effect |
|---|---|
| `X-Mock-Offers` | Offer count, ancillary count, or seat rows |
| `X-Mock-Delay` | Simulated latency for this request (`900ms` or `900ms~2s`) |
| `X-Mock-Airline` | Carrier profile to present |
| `Accept: application/json` | JSON projection instead of NDC XML |
| `Accept-Language` | Response language (`-lang-header` sets a vendor-specific header) |

Responses also echo `X-Mock-Translated` when a document was translated from another generation,
with `X-Mock-Translation-Coverage`: the share of element names the mapping accounts for. That is a
quick signal for a new mapping; schema validation is the real measure.

Responses echo `X-Mock-Offers`, `X-Mock-Airline` and `X-Mock-Version` with what was actually served.

---

## Configuration files

### `routes.json`

```json
{
  "default": { "offers": 0, "delay": "", "airline": "" },
  "routes": {
    "GRU-NAT": { "offers": 64,  "delay": "", "airline": "" },
    "MAD-GRU": { "offers": 320, "delay": "", "airline": "IB" }
  }
}
```

`offers: 0` uses whatever the reference holds. Precedence is header > route > default. An optional
`operations` map sets latency for everything after AirShopping (see
[Sizing and latency](#sizing-and-latency)).

### `airlines.json`

```json
{
  "airlines": {
    "IB": { "name": "...", "currency": "EUR", "hubs": ["MAD","BCN"],
            "fareScale": 1.25, "flightRange": [3000,7000] }
  },
  "fxToUSD": { "USD": 1.0, "EUR": 1.08, "COP": 0.00025 }
}
```

`fareScale` and `fxToUSD` are calibration knobs — set them from your own data. `hubs` marks which
routes a carrier would plausibly serve.

### `versions.json`

```json
{
  "versions": {
    "v192": { "iata": "19.2", "offerElement": "Offer",
              "idElements": ["OfferID","OfferItemID"], "stubs": "19.2" }
  }
}
```

---

## How it works

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for diagrams.

The reference response is sliced once at startup into a prefix (`DataLists`), the repeating
`<Offer>` blocks, and a suffix. Per request the mock:

1. Parses the request for origin, destination, dates and passenger types.
2. Resolves the version from the URL segment and picks the template (round trip vs one way, language).
3. Shifts every date, preserving relative offsets, so a next-day arrival stays next-day.
4. Re-encodes the offer identifiers so they agree with the request.
5. Emits the requested number of offers, recycling blocks with a price delta so none are identical.
6. Applies the carrier profile: code, currency, fare scale.
7. Assembles everything into one buffer and does a **single** `Write`.

Dates get one slot each rather than a single substitution because they are not independent
constants — a reference contains arrival dates one day after departure, and those have to move with
the requested date. Each is assigned to the outbound or inbound leg by proximity.

---

## Performance

Measured at 20 VUs on loopback.

| Offers | Payload | rps |
|---|---|---|
| 64 | 1.8 MB | 977 |
| 256 | 4.2 MB | 401 |
| 484 | 7.1 MB | 206 |

Inversely proportional to payload in all three: the ceiling is bandwidth (~1.7 GB/s), not response
assembly.

That single buffered write is the whole story:

| Strategy | rps on a 4.2 MB payload |
|---|---|
| Static, straight from memory | 403 |
| Dynamic, 6172 fragments written to the socket | 104 |
| Dynamic, assembled then written once | **405** |

Dynamism is free. Syscalls are not. Re-encoding 1496 identifiers per response costs nothing
measurable — it disappears into the write.

---

## CLI and MCP

Two clients ship with the mock, sharing one package. Both ask for the JSON projection, never NDC
XML: a terminal and an agent have the same problem with a multi-megabyte document.

An agent skill covering both clients and the mock's dials is in
[`skills/ndc-mock/SKILL.md`](skills/ndc-mock/SKILL.md). For Claude Code, link it into
`~/.claude/skills/`:

```bash
ln -s "$PWD/skills/ndc-mock" ~/.claude/skills/ndc-mock
```

### CLI

```bash
ndc search -from GRU -to NAT -depart 2026-09-30 -return 2026-10-02 -max 4
```

```
GRU -> NAT  2026-09-30 / 2026-10-02
64 offers (showing 4)

PRICE        ROUTING                          OFFER ID
USD 812   GRU-NAT XX1234 / NAT-GRU XX1235  <offer-id>
USD 1021  GRU-NAT XX1236 / NAT-GRU XX1237  <offer-id>
```

The rest of the flow chains off the returned identifier:

```bash
ndc price <offer-id>
ndc order <offer-id>        # order XX6761612ABCD  Opened  USD 812
ndc retrieve <order-id>
ndc cancel <order-id>       # history: Created -> Cancelled
ndc services <offer-id>
ndc seats <offer-id>
```

`-json` prints the raw projection instead of a summary, and works before or after the verb.
Endpoint and version come from `-url` / `-version` or `$NDC_URL` / `$NDC_VERSION`. Input is
validated before a request goes out:

```
error: origin "XX" is not a three-letter airport code
error: return date 2026-09-01 precedes departure 2026-09-30
error: 3 infants need at least as many adults, got 1
```

### MCP

`ndc-mcp` exposes the flow as MCP tools over stdio, so an agent can shop and book without knowing
anything about IATA XML.

```json
{
  "mcpServers": {
    "ndc": {
      "command": "/path/to/bin/ndc-mcp",
      "args": ["-url", "http://localhost:8090"]
    }
  }
}
```

| Tool | Purpose |
|---|---|
| `ndc_search` | Offers for a route and date, capped |
| `ndc_price` | Firm price for an offer |
| `ndc_order_create` | Book — changes state |
| `ndc_order_retrieve` | Order and its status |
| `ndc_order_cancel` | Cancel — changes state, not reversible |
| `ndc_services` | Ancillary catalogue |
| `ndc_seats` | Seat map |

Offers are capped (default 10) because an agent pays for every token it reads. The tools that change
state say so in their descriptions, so the agent asks before calling them. A failed call comes back
as tool content with `isError` rather than a protocol error, so the agent can see what went wrong
and retry.

The identifier returned by `ndc_search` is the real, self-describing one — it carries route, dates,
carrier, fare and passenger mix, which is what makes the chain work without server-side session
state.

---

## Running it

```bash
make image
docker run -p 8090:8090 -v /path/to/stubs:/stubs:ro ndc-mock:dev
```

The image (15.6 MB, distroless, non-root) carries the engine and the JSON configs, never a reference
response: that is provider data, so it is mounted at `/stubs`.

### Several replicas

Orders live in the memory of the replica that created them. Behind a load balancer the next call
for an order usually lands on another replica, so give each replica its own address and a name that
resolves to all of them:

```yaml
env:
  - name: POD_IP
    valueFrom: { fieldRef: { fieldPath: status.podIP } }
  - name: NDC_MOCK_ADVERTISE
    value: "$(POD_IP):8090"
  - name: NDC_MOCK_PEERS
    value: ndc-mock-headless.<namespace>.svc.cluster.local   # a headless Service
```

Each order identifier then carries its owner's address, and a replica that does not hold an order
forwards the request to the one that does. State stays in one place, so a cancel through one replica
is what a retrieve through another sees. A replica only forwards to addresses that `-peers`
resolves to: the identifier comes from the client, and without that check a crafted one would make
the mock a proxy to anything it can reach. A restarted replica loses its orders; the forwarded call
answers `502` saying so.

One replica needs none of this: leave both unset and identifiers keep the plain record-locator shape.

---

## Testing

```bash
make build         # server + both clients
make edge-check    # AirShopping: offer counts, identifiers, date shifting, overrides
make check-flow    # full flow in both formats, state transitions, 404s, carrier profile
make check-xlate   # translation: namespaces, renames, prefix safety, drops, coverage
make check-proxy   # proxy and record mode, and the fallback when upstream dies
make check-mcp     # MCP protocol, tool schemas, error semantics
make check-replicas # two replicas: an order created on one is served and changed through the other
make unit          # Go tests: latency quantiles, itinerary shapes, scoped renames, recording
make bruno         # Bruno collection
make test          # all of the above
```

`.github/workflows/ci.yml` runs vet, gofmt, the Go tests under `-race`, shellcheck, a secrets scan
and an image build on every push and pull request. The end-to-end suite needs a reference response,
which is never in the repository: it runs only when the `STUBS_URL` repository variable points at a
tarball of one (`STUBS_TOKEN`, a secret, is sent as a bearer if the URL needs it). Without it the
job is skipped, visibly, rather than reported green on an empty run.

Two of these checks are narrower than they look, and the scripts say so at the top:

- `check-proxy` points one instance at another, so it covers the wiring but **not a real provider** —
  redirects, compression, chunked transfer and TLS are untested. `tools/probe-proxy.sh` covers that
  (see [Getting a reference response](#getting-a-reference-response)).
- `check-mcp` speaks the protocol directly, so it covers the server's answers but **not a real MCP
  host**.

The Bruno collection lives in `bruno/` and asserts on response **headers** rather than bodies — a
multi-megabyte XML body crashes the Bruno CLI's embedded JS sandbox, and the echoed headers are the
better assertion anyway.

---

## Multi-city

A multi-city reference is any `air-shopping-mc*.xml` in the version's stubs directory. It serves every
request with the same **itinerary shape**, meaning the legs with their airports numbered by first
appearance:

| Request | Shape |
|---|---|
| `SCL-LIM, LIM-BOG, BOG-SCL` | `0-1,1-2,2-0` (a three-leg circuit) |
| `GRU-NAT, NAT-GIG, GIG-GRU` | `0-1,1-2,2-0`, served by the reference above |
| `GRU-NAT, NAT-GIG, GIG-MIA` | `0-1,1-2,2-3` (an open chain), needs its own reference |
| `GRU-SCL, LIM-GRU` | `0-1,2-0` (an open jaw), needs its own reference |

Airports are rewritten position by position, and dates move leg by leg, so a next-day arrival stays
next-day on every leg. The identifiers carry every leg, so pricing and ordering a multi-city offer
keeps all of them. A shape with no reference answers `501` and names the shapes that are loaded:

```
no multi-city reference of this itinerary's shape is loaded: shape 0-1,1-2,2-3, loaded 0-1,1-2,2-0
```

The references are one-way, return and one per multi-city shape because a reference cannot be
reshaped into another itinerary without inventing the trip.

A reference captured from a real provider may repeat an operated leg's `DatedOperatingLegID` inside
every segment that flies it. The 19.2 schema requires those IDs to be unique, so the mock renames the
repeats when it loads the reference (`ID_2`, `ID_3`) and logs how many it renamed. Nothing
references a 19.2 leg by ID, so no meaning changes. In 21.3 and later each operated leg is listed
once and referenced, so the repeats collapse into one entry.

---

## Getting a reference response

References live in `stubs/` (gitignored, since they are provider data; a symlink is fine), one
subdirectory per version, or flat for `-flat-version`: `default|en|es|pt/air-shopping-rs.xml` for one
way, `air-shopping-rt-rs.xml` for a return, and `air-shopping-mc*.xml` for multi-city.

`capture/capture.sh` records one from a live provider. You need it **once** — after that the mock
has no upstream dependency. Credentials are read from the environment and never written to disk;
captured responses are gitignored and are not distributed.

```bash
export NDC_BASE_URL=... TOKEN_KEY=... TOKEN_PWD=... API_KEY=...
bash capture/capture.sh GRU NAT 2026-09-30 2026-10-02
```

Providers validate strictly, and well-formed is not enough. One gateway rejects any `AirShoppingRQ`
lacking `ShoppingCriteria/ProgramCriteria` and a valid `TravelAgent` with `403 IATA Validation
Failed` even though the XML parses. `capture/rq-minimal.xml` is a shape that passes.

`tools/probe-proxy.sh` does the same through the mock, which exercises proxy mode against the real
provider. It sends read-only calls only: AirShopping round trip, one way and multi-city, then
OfferPrice on the first offer, never OrderCreate. For each call it reports whether the provider
answered or the mock fell back to synthetic, along with the status, size, offer count and any provider
error. It records the exchanges in `capture/rs/proxy/`, which is gitignored, and validates them
against the XSD when `XSD_DIR` is set. Credentials and agency identity come from the environment:

```bash
NDC_BASE_URL=... TOKEN_KEY=... TOKEN_PWD=... API_KEY=... \
AGENCY_ID=... IATA_NUMBER=... AGENT_ID=... ACCOUNT_CODE=... \
MC="SCL-LIM,LIM-BOG,BOG-SCL" XSD_DIR=/path/to/19_2_schemas tools/probe-proxy.sh
```

`capture/transform-airline.py` rewrites a capture as a different carrier offline, if you would
rather bake carrier variants into files than resolve them per request.

---

## What it does not do

- **No live inventory.** Flight numbers, schedules and availability come from the reference. Routes
  and fares are *plausible for the carrier*, not that carrier's real availability on a date. Correct
  for load testing; wrong for contract testing.
- **Multi-city only in the shapes you have references for.** See [Multi-city](#multi-city).
- **No real fare rules.** Penalties, brand conditions and tax breakdowns are structural, not
  semantic.
- **Order state is in memory.** It does not survive a restart. Several replicas work by forwarding
  to the owner (see [Several replicas](#several-replicas)), not by sharing a store.
- **Latency is uncalibrated.** Zero until you set it.
- **Installments is a provider extension** outside the IATA schemas, so it is not schema-checked
  and keeps one shape under every version.

---

## Security

See [docs/SECURITY.md](docs/SECURITY.md). In short: this is a test double with **no
authentication** by design — do not expose it to an untrusted network. Resource ceilings
(`-max-offers`, `-max-delay`) and server timeouts are set so a single request cannot exhaust it.

---

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
