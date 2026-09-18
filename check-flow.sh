#!/usr/bin/env bash
# Exercises the full flow end to end, in both wire formats.
# Covers what fails silently: state transitions that do not stick, identifiers
# that stop agreeing with the request, and the JSON projection drifting from XML.
set -uo pipefail
H=${1:-http://localhost:8090}
V=${VER:-v192}
B="$H/ndc/$V"
D="$(dirname "$0")/dataset"
t=$(mktemp -d); trap 'rm -rf $t' EXIT
fail() { echo "FAIL: $*"; exit 1; }

post() { curl -sf -o "$2" -w '%{http_code}' -X POST "$B/$1" -H 'Content-Type: application/xml' "${@:3}"; }

# 1. AirShopping in both formats must agree on the offer count.
code=$(post "airshopping" $t/as.xml --data-binary @$D/gru-nat-64.xml)
[ "$code" = 200 ] || fail "airshopping returned $code"
xmllint --noout $t/as.xml || fail "airshopping XML is not well-formed"
xmlCount=$(grep -oE '<OfferID>' $t/as.xml | wc -l | tr -d ' ')

code=$(post "airshopping?format=json" $t/as.json --data-binary @$D/gru-nat-64.xml)
[ "$code" = 200 ] || fail "airshopping JSON returned $code"
jsonCount=$(python3 -c "import json;print(json.load(open('$t/as.json'))['offerCount'])")
[ "$xmlCount" = "$jsonCount" ] || fail "offer count differs: XML $xmlCount vs JSON $jsonCount"

# 2. OfferPrice must echo the offer it was handed.
OID=$(grep -oE '<OfferID>[^<]+' $t/as.xml | sed -n '1s|<OfferID>||p')
printf '<?xml version="1.0"?><IATA_OfferPriceRQ><Request><OfferID>%s</OfferID></Request></IATA_OfferPriceRQ>' "$OID" > $t/op.xml
code=$(post "offerprice" $t/op.rs.xml --data-binary @$t/op.xml)
[ "$code" = 200 ] || fail "offerprice returned $code"
xmllint --noout $t/op.rs.xml || fail "offerprice XML is not well-formed"
grep -qF "$OID" $t/op.rs.xml || fail "offerprice did not echo the offer it was given"

# 3. OrderCreate must mint an order that is then retrievable.
printf '<?xml version="1.0"?><IATA_OrderCreateRQ><Request><OfferID>%s</OfferID></Request></IATA_OrderCreateRQ>' "$OID" > $t/oc.xml
code=$(post "ordercreate?format=json" $t/oc.json --data-binary @$t/oc.xml)
[ "$code" = 201 ] || fail "ordercreate returned $code, expected 201"
ORD=$(python3 -c "import json;print(json.load(open('$t/oc.json'))['orderId'])")
[ -n "$ORD" ] || fail "ordercreate returned no orderId"

mkrq() { printf '<?xml version="1.0"?><IATA_OrderRetrieveRQ><Request><OrderID>%s</OrderID></Request></IATA_OrderRetrieveRQ>' "$ORD" > $t/rq.xml; }
mkrq
code=$(post "orderretrieve?format=json" $t/or.json --data-binary @$t/rq.xml)
[ "$code" = 200 ] || fail "orderretrieve returned $code"
st=$(python3 -c "import json;print(json.load(open('$t/or.json'))['status'])")
[ "$st" = "Opened" ] || fail "new order is '$st', expected Opened"

# 4. Ancillaries and seats must honour the size override.
code=$(post "servicelist?format=json" $t/sv.json -H 'X-Mock-Offers: 25' --data-binary @$t/op.xml)
[ "$code" = 200 ] || fail "servicelist returned $code"
n=$(python3 -c "import json;print(json.load(open('$t/sv.json'))['count'])")
[ "$n" = 25 ] || fail "servicelist returned $n services, expected 25"

code=$(post "seatavailability?format=json" $t/se.json -H 'X-Mock-Offers: 10' --data-binary @$t/op.xml)
[ "$code" = 200 ] || fail "seatavailability returned $code"
n=$(python3 -c "import json;print(json.load(open('$t/se.json'))['count'])")
[ "$n" = 60 ] || fail "seatavailability returned $n seats, expected 60 (10 rows x 6)"

# 5. Servicing must change state, and the change must persist.
for op in orderchange ordercancel; do
  code=$(post "$op" $t/x.xml --data-binary @$t/rq.xml)
  [ "$code" = 200 ] || fail "$op returned $code"
  xmllint --noout $t/x.xml || fail "$op XML is not well-formed"
done
code=$(post "orderretrieve?format=json" $t/or2.json --data-binary @$t/rq.xml)
st=$(python3 -c "import json;print(json.load(open('$t/or2.json'))['status'])")
[ "$st" = "Cancelled" ] || fail "after cancel the order is '$st', expected Cancelled"
hist=$(python3 -c "import json;print(len(json.load(open('$t/or2.json'))['history']))")
[ "$hist" -ge 3 ] || fail "history has $hist entries, expected the full transition trail"

# 6. An unknown order must 404 in both formats, not invent one.
printf '<?xml version="1.0"?><IATA_OrderRetrieveRQ><Request><OrderID>NOPE123</OrderID></Request></IATA_OrderRetrieveRQ>' > $t/bad.xml
for suffix in "" "?format=json"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/orderretrieve$suffix" \
    -H 'Content-Type: application/xml' --data-binary @$t/bad.xml)
  [ "$code" = 404 ] || fail "unknown order returned $code for '$suffix', expected 404"
done

# 7. Carrier profile must reach the ordering half of the flow too.
code=$(post "offerprice?format=json" $t/ib.json -H 'X-Mock-Airline: IB' --data-binary @$t/op.xml)
cur=$(python3 -c "import json;print(json.load(open('$t/ib.json'))['pricedOffer']['price']['currency'])")
[ "$cur" = "EUR" ] || fail "carrier profile did not apply: currency is $cur, expected EUR"

echo "OK: shopping+ordering in XML and JSON, state persists, size overrides honoured, 404 on unknown order, carrier profile applied"
