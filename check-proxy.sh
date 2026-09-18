#!/usr/bin/env bash
# Exercises proxy and record mode by pointing one instance at another, so the
# path is covered without needing a live provider.
#
# What this does NOT cover: a real upstream, where redirects, compression,
# chunked transfer and TLS come into play. Treat a pass here as "the wiring
# works", not as "proxying a real airline works".
set -uo pipefail
R="$(cd "$(dirname "$0")" && pwd)"
STUBS=${STUBS:-$R/stubs}
UP_PORT=${UP_PORT:-8094}
PX_PORT=${PX_PORT:-8095}
t=$(mktemp -d)
fail() { echo "FAIL: $*"; exit 1; }

up="" px=""
cleanup() { [ -n "$up" ] && kill "$up" 2>/dev/null; [ -n "$px" ] && kill "$px" 2>/dev/null; rm -rf "$t"; }
trap cleanup EXIT

common=(-stubs "$STUBS" -config "$R/routes.json" -versions "$R/versions.json"
        -airlines "$R/airlines.json" -translations "$R/translations")

"$R/edge/ndc-edge-mock" -addr ":$UP_PORT" "${common[@]}" > "$t/up.log" 2>&1 &
up=$!
"$R/edge/ndc-edge-mock" -addr ":$PX_PORT" "${common[@]}" \
  -proxy "http://localhost:$UP_PORT" -record "$t/rec" > "$t/px.log" 2>&1 &
px=$!

wait_up() {
  for _ in $(seq 60); do curl -sf -o /dev/null "http://localhost:$1/__health" && return 0; sleep 0.5; done
  return 1
}
wait_up "$UP_PORT" || fail "upstream did not start: $(tail -3 "$t/up.log")"
wait_up "$PX_PORT" || fail "proxy did not start: $(tail -3 "$t/px.log")"

grep -qi 'proxy+record' "$t/px.log" || fail "proxy instance did not announce record mode"

# 1. A proxied request must come back from upstream and say so.
code=$(curl -s -o "$t/out.xml" -D "$t/hdr.txt" -w '%{http_code}' -X POST \
  "http://localhost:$PX_PORT/ndc/v192/airshopping" -H 'Content-Type: application/xml' \
  -H 'X-Mock-Offers: 5' --data-binary @"$R/dataset/gru-nat-64.xml")
[ "$code" = 200 ] || fail "proxied request returned $code"
grep -qi 'x-mock-mode: proxy' "$t/hdr.txt" || fail "response was not marked as proxied"
xmllint --noout "$t/out.xml" || fail "proxied document is not well-formed"

# Upstream headers must survive the hop: without them a caller cannot tell what
# it was served.
grep -qi 'x-mock-offers:' "$t/hdr.txt" || fail "upstream headers were not forwarded back"

# 2. The exchange must be on disk, named after the route it carried.
ls "$t/rec"/airshopping-GRU-NAT-*-rs.xml >/dev/null 2>&1 || fail "no recording named for the route: $(ls "$t/rec" 2>/dev/null)"
ls "$t/rec"/airshopping-GRU-NAT-*-rq.xml >/dev/null 2>&1 || fail "the request side was not recorded"
rs=$(ls "$t/rec"/airshopping-GRU-NAT-*-rs.xml | head -1)
cmp -s "$rs" "$t/out.xml" || fail "the recording does not match what was served"

# 3. A second request must not overwrite the first: captures accumulate.
curl -sf -o /dev/null -X POST "http://localhost:$PX_PORT/ndc/v192/airshopping" \
  -H 'Content-Type: application/xml' -H 'X-Mock-Offers: 5' \
  --data-binary @"$R/dataset/gru-gig-256.xml" || fail "second proxied request failed"
n=$(ls "$t/rec"/*-rs.xml 2>/dev/null | wc -l | tr -d ' ')
[ "$n" -ge 2 ] || fail "recordings did not accumulate: $n file(s)"

# 4. With the upstream gone the proxy must degrade to synthetic, not fail the
#    load test outright.
kill "$up" 2>/dev/null; up=""
sleep 1
code=$(curl -s -o "$t/fb.xml" -D "$t/fbh.txt" -w '%{http_code}' -X POST \
  "http://localhost:$PX_PORT/ndc/v192/airshopping" -H 'Content-Type: application/xml' \
  -H 'X-Mock-Offers: 5' --data-binary @"$R/dataset/gru-nat-64.xml")
[ "$code" = 200 ] || fail "fallback returned $code, expected the synthetic response"
grep -qi 'x-mock-mode: synthetic-fallback' "$t/fbh.txt" || fail "fallback was not marked as such"
xmllint --noout "$t/fb.xml" || fail "fallback document is not well-formed"

# A failed upstream must not be recorded as if it were a real capture.
n2=$(ls "$t/rec"/*-rs.xml 2>/dev/null | wc -l | tr -d ' ')
[ "$n2" = "$n" ] || fail "a synthetic fallback was recorded as a capture"

echo "OK: proxied and marked, upstream headers forwarded, $n captures on disk, fallback to synthetic when upstream dies"
