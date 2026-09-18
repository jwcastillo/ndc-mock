#!/usr/bin/env bash
# Validates a generation end to end against its official XSD, requests and
# responses alike: AirShopping over every reference and a recycled offer count,
# then the flow after it (OfferPrice, ServiceList, SeatAvailability,
# OrderCreate/Retrieve/Change/Reshop/Cancel)
# for a single adult on a round trip and a family on one way under another
# carrier profile.
#
#   tools/validate-translation.sh v241 /path/to/24_1_distribution_schemas [host]
#
# Requests come from dataset/rq/<generation>/, each file from the newest
# generation not later than the one under test (24.1 only overrides the one
# request it renamed), and are schema-checked before they are sent, so a
# failure is the mock's and not the fixture's. The schemas are
# IATA-licensed and are not in the repository - download them from IATA and
# point at the unpacked directory. xmllint reports only the first error under
# each parent, so a fix can reveal errors that were hidden behind it.
set -uo pipefail
V=${1:?version segment, e.g. v241}; XSD=${2:?directory holding the IATA_*.xsd files}
H=${3:-http://localhost:8090}
R="$(cd "$(dirname "$0")/.." && pwd)"
t=$(mktemp -d); trap 'rm -rf $t' EXIT

IATA=$(python3 -c "import json,sys;print(json.load(open('$R/versions.json'))['versions']['$V']['iata'])") ||
  { echo "unknown version $V"; exit 1; }
# rq <name>: the fixture from the newest generation directory not later than this one.
rq() {
  python3 - "$R/dataset/rq" "$IATA" "$1" <<'PY'
import os, sys
base, want, name = sys.argv[1:]
key = lambda v: tuple(int(x) for x in v.split('.'))
for d in sorted(os.listdir(base), key=key, reverse=True):
    if key(d) <= key(want) and os.path.exists(f'{base}/{d}/{name}.xml'):
        print(f'{base}/{d}/{name}.xml'); break
PY
}
if [ "$IATA" = 19.2 ]; then SHOP="$R/dataset/gru-nat-64.xml"; else SHOP=$(rq airshopping); fi

# Variants of the shopping request: one way, a family, another route.
python3 - "$SHOP" "$t" <<'PY'
import re, sys
src, out = sys.argv[1], sys.argv[2]
s = open(src).read()
legs = re.findall(r'<OriginDestCriteria>.*?</OriginDestCriteria>', s, re.S)
ow = s.replace(legs[1], '') if len(legs) > 1 else s
fam = ''.join(f'<Pax><PaxID>{p}_{n}</PaxID><PTC>{p}</PTC></Pax>' for p, n in [('ADT', 1), ('ADT', 2), ('CHD', 1), ('INF', 1)])
open(f'{out}/rt.xml', 'w').write(s)
open(f'{out}/ow.xml', 'w').write(ow)
open(f'{out}/family.xml', 'w').write(re.sub(r'<(Paxs|PaxList)>.*?</\1>', lambda m: f'<{m.group(1)}>{fam}</{m.group(1)}>', ow, flags=re.S))
open(f'{out}/rio.xml', 'w').write(s.replace('NAT', 'RIO'))
# A three-leg circuit, the shape a multi-city reference usually has.
tpl = legs[0]
def leg(o, d, date):
    x = re.sub(r'(<OriginDepCriteria>.*?<Date>)[^<]+', r'\g<1>' + date, tpl, flags=re.S)
    x = re.sub(r'(<OriginDepCriteria>.*?<IATA_LocationCode>)[A-Z]{3}', r'\g<1>' + o, x, flags=re.S)
    return re.sub(r'(<DestArrivalCriteria>.*?<IATA_LocationCode>)[A-Z]{3}', r'\g<1>' + d, x, flags=re.S)
mc = s.replace(''.join(legs), leg('GRU', 'NAT', '2026-12-01') + leg('NAT', 'GIG', '2026-12-05') + leg('GIG', 'GRU', '2026-12-12'))
if ''.join(legs) not in s:  # legs separated by whitespace
    mc = s.replace(legs[1], '').replace(legs[0], leg('GRU', 'NAT', '2026-12-01') + leg('NAT', 'GIG', '2026-12-05') + leg('GIG', 'GRU', '2026-12-12'))
open(f'{out}/mc.xml', 'w').write(mc)
PY

failed=0
root_of() { grep -oE '<([A-Za-z]+:)?IATA_[A-Za-z]+' "$1" | head -1 | sed 's/.*IATA_/IATA_/'; }

schema_ok() { # file label
  if xmllint --noout --schema "$XSD/$(root_of "$1").xsd" "$1" 2>"$t/err" >/dev/null; then return 0; fi
  echo "$2: INVALID, $(grep -c 'validity error' "$t/err") errors"
  [ "$failed" = 1 ] || python3 - "$t/err" <<'PY'
import re, sys, collections
c = collections.Counter()
for l in open(sys.argv[1]):
    m = re.search(r"Element '([^']+)'(.*)", l)
    if m:
        msg = re.sub(r"\{[^}]*\}|\[.*?\]|'[^']*'|\([^)]*\)", '…', m.group(2))[:100]
        c[re.sub(r'\{[^}]*\}', '', m.group(1)) + ' ' + msg] += 1
for k, v in c.most_common(15):
    print(f'{v:8} {k}')
PY
  failed=1
  return 1
}

send() { # label operation request-file [curl args]; the response lands in $t/rs.xml
  local label=$1 op=$2 body=$3; shift 3
  schema_ok "$body" "$label request" || return 1
  curl -sf -o "$t/rs.xml" -X POST "$H/ndc/$V/$op" "$@" --data-binary @"$body" ||
    { echo "$label: no response from $H/ndc/$V/$op"; failed=1; return 1; }
  schema_ok "$t/rs.xml" "$label" || return 1
}

shop() { # label request [curl args]
  local label=$1; shift
  send "$label" airshopping "$@" && echo "$label: valid ($(grep -c '<OfferID>' "$t/rs.xml") offers)"
}

shop round-trip          "$t/rt.xml"
shop round-trip-500      "$t/rt.xml" -H 'X-Mock-Offers: 500'
shop one-way             "$t/ow.xml"
shop one-way-pt-300      "$t/ow.xml" -H 'X-Mock-Offers: 300' -H 'Accept-Language: pt'
shop one-way-es          "$t/ow.xml" -H 'Accept-Language: es'
shop one-way-en          "$t/ow.xml" -H 'Accept-Language: en'
shop round-trip-gru-rio  "$t/rio.xml"

# Multi-city needs a reference of the same itinerary shape; without one the
# mock answers 501 and there is nothing to validate.
have_mc() {
  [ "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$H/ndc/$V/airshopping" --data-binary @"$t/mc.xml")" = 200 ]
}
if have_mc; then shop multi-city "$t/mc.xml"; else echo "multi-city: skipped, no multi-city reference of this shape loaded"; fi

fill() { # template -> $t/rq.xml, from the values gathered so far
  python3 - "$1" "$t/rq.xml" "$t/vals" <<'PY'
import sys
tpl, out, vals = sys.argv[1:]
v = dict(l.rstrip('\n').split('=', 1) for l in open(vals))
pax = v['PAX'].split()
v['PAX_REFS'] = ''.join(f'<PaxRefID>{p}</PaxRefID>' for p in pax)
v['PAXES'] = ''.join(f'<Pax><PaxID>{p}</PaxID><PTC>{p.split("_")[0]}</PTC></Pax>' for p in pax)
v['PAX_LIST'] = '<DataLists><PaxList>' + v['PAXES'] + '</PaxList></DataLists>'
s = open(tpl).read()
for k, x in v.items():
    s = s.replace(f'__{k}__', x)
open(out, 'w').write(s)
PY
}

flow() { # label shopping-request [curl args]
  local label=$1 body=$2; shift 2
  send "$label/airshopping" airshopping "$body" "$@" || return
  {
    echo "OFFER_ID=$(grep -oE '<OfferID>[^<]+' "$t/rs.xml" | sed -n '1s|<OfferID>||p')"
    echo "OFFER_ITEM_ID=$(grep -oE '<OfferItemID>[^<]+' "$t/rs.xml" | sed -n '1s|<OfferItemID>||p')"
    echo "OWNER=$(grep -oE '<OwnerCode>[^<]+' "$t/rs.xml" | sed -n '1s|<OwnerCode>||p')"
    echo "PAX=$(grep -oE '<PaxID>[^<]+' "$body" | sed 's|<PaxID>||' | tr '\n' ' ')"
  } > "$t/vals"
  fill "$(rq offer-price)";  send "$label/offerprice" offerprice "$t/rq.xml" "$@" && echo "$label/offerprice: valid"
  fill "$(rq service-list)";      send "$label/servicelist" servicelist "$t/rq.xml" -H 'X-Mock-Offers: 20' "$@" && echo "$label/servicelist: valid"
  fill "$(rq seat-availability)"; send "$label/seatavailability" seatavailability "$t/rq.xml" -H 'X-Mock-Offers: 12' "$@" && echo "$label/seatavailability: valid"
  fill "$(rq order-create)"; send "$label/ordercreate" ordercreate "$t/rq.xml" "$@" && echo "$label/ordercreate: valid"
  echo "ORDER_ID=$(grep -oE '<OrderID>[^<]+' "$t/rs.xml" | sed -n '1s|<OrderID>||p')" >> "$t/vals"
  fill "$(rq order-retrieve)"; send "$label/orderretrieve" orderretrieve "$t/rq.xml" && echo "$label/orderretrieve: valid"
  fill "$(rq order-change)";   send "$label/orderchange" orderchange "$t/rq.xml" && echo "$label/orderchange: valid"
  fill "$(rq order-reshop)";   send "$label/orderreshop" orderreshop "$t/rq.xml" -H 'X-Mock-Offers: 3' && echo "$label/orderreshop: valid"
  # From 21.3 there is no OrderCancelRQ: a cancel is an OrderChangeRQ.
  if [ "$IATA" = 19.2 ]; then fill "$(rq order-cancel)"; else fill "$(rq order-change)"; fi
  send "$label/ordercancel" ordercancel "$t/rq.xml" && echo "$label/ordercancel: valid"
}
flow round-trip "$t/rt.xml"
flow one-way-family "$t/family.xml" -H 'X-Mock-Airline: IB'
if have_mc; then flow multi-city "$t/mc.xml"; fi
exit $failed
