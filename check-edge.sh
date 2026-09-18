#!/usr/bin/env bash
# Smoke test. Covers the three things that fail silently if they break:
# identifiers must be re-encoded to agree with the request, dates must shift
# while preserving their offsets, and the offer count must be the configured one.
set -euo pipefail
H=${1:-http://localhost:8090}
D=$(dirname "$0")/dataset
t=$(mktemp -d); trap 'rm -rf $t' EXIT

post() { curl -sf -o "$2" -w '%{http_code}' -X POST "$H/ndc/${VER:-v192}/airshopping" \
  -H 'Content-Type: application/xml' "${@:3}" --data-binary @"$1"; }

# sed -n '1s//p' rather than head -1: head closes the pipe, and under pipefail that is a SIGPIPE.
firstOffer() { grep -oE '<OfferID>[^<]*' "$1" | sed -n '1s|<OfferID>||p' \
  | tr '|' '\n' | while read -r s; do base64 -d <<<"$s" 2>/dev/null; echo; done; }
countOffers() { grep -c '<OfferID>' "$1"; }

# 1. Each route returns its configured offer count, all distinct (blocks are
#    recycled with a price delta, not duplicated).
for spec in gru-nat-64:64 gru-gig-256:256 gru-rio-484:484; do
  f=${spec%:*}; want=${spec#*:}
  [ "$(post $D/$f.xml $t/rs.xml)" = 200 ] || { echo "FAIL: $f did not return 200"; exit 1; }
  xmllint --noout $t/rs.xml || { echo "FAIL: $f is not well-formed XML"; exit 1; }
  got=$(countOffers $t/rs.xml)
  [ "$got" = "$want" ] || { echo "FAIL: $f returned $got offers, expected $want"; exit 1; }
  uniq=$(grep -oE '<OfferID>[^<]*' $t/rs.xml | sort -u | wc -l | tr -d ' ')
  [ "$uniq" = "$want" ] || { echo "FAIL: $f has $uniq distinct identifiers out of $want"; exit 1; }
done

# 2. A round-trip identifier carries both legs and the requested passenger mix.
post $D/gru-nat-64.xml $t/rs.xml > /dev/null
dec=$(firstOffer $t/rs.xml)
for want in 'GRU,NAT,2026-09-30.NAT,GRU,2026-10-02' 'PX=1/0/0'; do
  grep -qF "$want" <<<"$dec" || { echo "FAIL: identifier does not carry '$want'"; echo "$dec"; exit 1; }
done
grep -qF 'BOG,MCO' <<<"$dec" && { echo "FAIL: identifier still carries the reference itinerary"; exit 1; }

# 3. Dates shift while preserving offsets: the reference has next-day arrivals,
#    so 10-01 (outbound+1) and 10-03 (inbound+1) must appear, and no date from
#    the original reference may survive.
dates=$(grep -oE '[0-9]{4}-[0-9]{2}-[0-9]{2}' $t/rs.xml | sort -u | tr '\n' ' ')
for want in 2026-09-30 2026-10-01 2026-10-02 2026-10-03; do
  grep -qF "$want" <<<"$dates" || { echo "FAIL: missing date $want; got: $dates"; exit 1; }
done
grep -qE '2026-0[56]-' <<<"$dates" && { echo "FAIL: reference dates survived: $dates"; exit 1; }

# 4. Header override, so a load test can sweep values without editing config.
post $D/gru-nat-64.xml $t/rs.xml -H 'X-Mock-Offers: 7' > /dev/null
[ "$(countOffers $t/rs.xml)" = 7 ] || { echo "FAIL: X-Mock-Offers was not honoured"; exit 1; }

# 5. A version with no reference response of its own must be served by
#    translation and must SAY so: a 200 with no translation header would mean
#    it quietly served another generation's payload as if it were native.
hdr=$(curl -s -o /dev/null -D - -X POST "$H/ndc/v213/airshopping" \
  -H 'Content-Type: application/xml' -H 'X-Mock-Offers: 2' \
  --data-binary @$D/gru-nat-64.xml)
grep -qi 'x-mock-translated:' <<<"$hdr" || { echo "FAIL: v213 answered without declaring a translation"; exit 1; }
grep -qi 'x-mock-translation-coverage:' <<<"$hdr" || { echo "FAIL: translation did not report coverage"; exit 1; }

# 6. From 21.3 the root lives in the shared message namespace and the body in
#    the common-types one; a per-message 2021.3 namespace does not exist.
for v in v213 v241 v244; do
  curl -s -o "$t/x.xml" -X POST "$H/ndc/$v/airshopping" -H 'X-Mock-Offers: 2' --data-binary @$D/gru-nat-64.xml
  root=$(head -c 600 "$t/x.xml" | grep -o '<m:IATA_AirShoppingRS [^>]*>')
  grep -q 'xmlns="http://www.iata.org/IATA/2015/EASD/00/IATA_OffersAndOrdersCommonTypes"' <<<"$root" &&
    grep -q 'xmlns:m="http://www.iata.org/IATA/2015/EASD/00/IATA_OffersAndOrdersMessage"' <<<"$root" ||
    { echo "FAIL: $v root is not in the 21.3+ namespace layout"; exit 1; }
  xmllint --noout "$t/x.xml" || { echo "FAIL: $v translation is not well-formed"; exit 1; }
done

echo "OK: 64/256/484 distinct offers, round-trip identifier re-encoded, dates shifted, header override, v213/v241/v244 translated, declared and namespaced"
