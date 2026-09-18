# capture/

Records one reference response from a live NDC provider. You need this **once** — from then on the
mock has no upstream dependency.

## Usage

Credentials are read from the environment and never written to disk. Captured responses are
gitignored and are not part of this repository.

```bash
export TOKEN_KEY=... TOKEN_PWD=... API_KEY=...
bash capture/capture.sh GRU NAT 2026-09-30 2026-10-02
# -> capture/rs/airshopping-GRU-NAT.xml
```

## Request validation

Providers validate strictly, and a well-formed document is not enough. One gateway rejects any
`AirShoppingRQ` that lacks `ShoppingCriteria/ProgramCriteria` (account id and owning carrier) plus a
valid `TravelAgent`, answering `403 IATA Validation Failed` even though the XML parses.

`rq-minimal.xml` is a request shape that passes. Adapt the agency identifiers and account code to
your own provider.

## Scope

`AirShopping` is read-only and safe to script. The rest of the flow is not:

- `OfferPrice` needs an offer identifier from a prior `AirShopping` — it has to be chained.
- `OrderCreate` and servicing operations **mutate real state**. Capture those one at a time and
  deliberately, never in a loop.

## Deriving other carriers

`transform-airline.py` rewrites a captured response as a different carrier, converting currency and
fare magnitude from `airlines.json` while preserving the fare distribution. This is how the mock
covers carriers you have no access to.

```bash
python3 capture/transform-airline.py capture/rs/airshopping-GRU-NAT.xml IB > iberia.xml
```
