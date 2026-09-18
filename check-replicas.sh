#!/usr/bin/env bash
# Two replicas behind no balancer: an order created on one must be retrievable
# and changeable through the other, and a crafted owner outside -peers must not
# be forwarded to.
set -uo pipefail
STUBS=${STUBS:-stubs}
cd "$(dirname "$0")" || exit 1
D=dataset; t=$(mktemp -d)
fail() { echo "FAIL: $*"; exit 1; }
start() {
  NDC_MOCK_ADVERTISE=127.0.0.1:$1 NDC_MOCK_PEERS=localhost ./edge/ndc-edge-mock -addr :$1 -stubs "$STUBS" \
    -config routes.json -versions versions.json -airlines airlines.json -translations translations >$t/$1.log 2>&1 &
  echo $!
}
A=$(start 8191); B=$(start 8192)
trap 'kill $A $B 2>/dev/null; rm -rf $t' EXIT
for p in 8191 8192; do
  for _ in $(seq 30); do curl -sf -o /dev/null localhost:$p/__health && break; sleep 0.3; done
done
rq() { printf '<?xml version="1.0"?><IATA_Order%sRQ><Request><OrderID>%s</OrderID></Request></IATA_Order%sRQ>' "$1" "$2" "$1"; }
post() { curl -s -o "$3" -w '%{http_code}' -X POST "localhost:$1/ndc/v192/$2" --data-binary @-; }

curl -sf -X POST localhost:8191/ndc/v192/airshopping --data-binary @$D/gru-nat-64.xml -o $t/as.xml || fail "airshopping"
OID=$(grep -oE '<OfferID>[^<]+' $t/as.xml | sed -n '1s|<OfferID>||p')
code=$(printf '<IATA_OrderCreateRQ><Request><OfferID>%s</OfferID></Request></IATA_OrderCreateRQ>' "$OID" |
  post 8191 'ordercreate?format=json' $t/oc.json)
[ "$code" = 201 ] || fail "create on A returned $code"
ORD=$(python3 -c "import json;print(json.load(open('$t/oc.json'))['orderId'])")
[[ $ORD == *.* ]] || fail "order id $ORD carries no owner"

code=$(rq Cancel "$ORD" | post 8192 'ordercancel?format=json' $t/cx.json)
[ "$code" = 200 ] || fail "cancel via B returned $code"
code=$(rq Retrieve "$ORD" | post 8191 'orderretrieve?format=json' $t/rt.json)
st=$(python3 -c "import json;print(json.load(open('$t/rt.json'))['status'])")
[ "$st" = Cancelled ] || fail "cancel through B did not stick on A: '$st'"

# Owner outside -peers: 404, never a request to that address.
evil="XX1234567ABCD.$(printf '10.255.255.1:80' | base64 | tr -d '=' | tr '+/' '-_')"
code=$(rq Retrieve "$evil" | post 8192 'orderretrieve?format=json' $t/ev.json)
[ "$code" = 404 ] || fail "crafted owner returned $code, expected 404"
echo "ok: order created on A is served and changed through B; foreign owners refused"
