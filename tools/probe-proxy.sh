#!/usr/bin/env bash
# Runs the mock in proxy+record mode against a live NDC provider and sends a
# read-only flow through it: AirShopping round trip, one way and multi-city,
# then OfferPrice on the first round-trip offer. Never OrderCreate - that
# mutates real state.
#
# Reports, per call, whether the provider answered (X-Mock-Mode: proxy) or the
# mock fell back to synthetic, the status, size, offer count and any provider
# error, and validates every recorded response against the XSD when XSD_DIR
# points at the provider generation's unpacked schemas.
#
# Credentials and agency identity come from the environment only and are
# never printed. OPERATIONS names the provider's route layout (the file given
# to the mock's -operations) and TOKEN_PATH its token endpoint; ACCOUNT_CODE
# and CARRIER add ShoppingCriteria/ProgramCriteria for gateways that need it.
#
#   NDC_BASE_URL=https://provider.example tools/probe-proxy.sh
#   MC="SCL-LIM,LIM-BOG,BOG-SCL" tools/probe-proxy.sh      # multi-city legs
#   HEADERS="X-Vendor-Country: BR; X-Vendor-Op: {op}; X-Vendor-Trace: {uuid}" tools/probe-proxy.sh
#
# HEADERS carries a provider's own headers: sent on every call and added to
# what the proxy forwards. {op} becomes the operation (AirShopping,
# OfferPrice) and {uuid} a fresh identifier per call.
#
# Recorded exchanges land in capture/rs/proxy/ (gitignored: provider data).
set -uo pipefail
R="$(cd "$(dirname "$0")/.." && pwd)"
: "${NDC_BASE_URL:?set the provider base URL}"
for v in TOKEN_KEY TOKEN_PWD API_KEY AGENCY_ID IATA_NUMBER AGENT_ID POS_CITY POS_COUNTRY; do
  [ -n "${!v:-}" ] || { echo "missing $v in the environment"; exit 1; }
done
: "${NDC_VERSION:=v192}" "${LANG_CODE:=en}" "${TOKEN_PATH:=/oauth/token}" "${ACCOUNT_CODE:=}" "${CARRIER:=}"
: "${RT:=GRU-NAT}" "${MC:=SCL-LIM,LIM-BOG,BOG-SCL}" "${PORT:=8197}" "${STUBS:=$R/stubs}" "${OPERATIONS:=}"

# path <handler>: where the provider serves that operation.
path() {
  [ -n "$OPERATIONS" ] || { echo "$1"; return; }
  python3 -c "import json,sys;r=json.load(open(sys.argv[1]))['routes'];print(next(p for p,h in r.items() if h==sys.argv[2]))" "$OPERATIONS" "$1"
}
REC="$R/capture/rs/proxy"; mkdir -p "$REC"
t=$(mktemp -d)
IFS=';' read -ra extra <<<"${HEADERS:-}"
forward="Authorization,X-Api-Key,Accept-Language"
for h in "${extra[@]}"; do n=$(sed 's/:.*//; s/^ *//; s/ *$//' <<<"$h"); [ -n "$n" ] && forward+=",$n"; done

"$R/edge/ndc-edge-mock" -addr ":$PORT" -stubs "$STUBS" -config "$R/routes.json" \
  -versions "$R/versions.json" -airlines "$R/airlines.json" -translations "$R/translations" \
  -proxy "$NDC_BASE_URL" -record "$REC" ${OPERATIONS:+-operations "$OPERATIONS"} \
  -forward-headers "$forward" \
  > "$t/mock.log" 2>&1 &
mock=$!
trap 'kill $mock 2>/dev/null; rm -rf $t' EXIT
for _ in $(seq 40); do curl -sf -o /dev/null "localhost:$PORT/__health" && break; sleep 0.25; done

# The token comes from the provider directly: the mock's own token endpoint is
# a stand-in and is not proxied.
tok=$(curl -s -m 30 -X POST "$NDC_BASE_URL$TOKEN_PATH" -H "x-api-key: $TOKEN_KEY" -H "Authorization: $TOKEN_PWD" \
  -H 'Content-Type: application/x-www-form-urlencoded' -d 'grant_type=client_credentials' |
  python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null)
[ -n "$tok" ] || { echo "token: the provider did not issue one (check TOKEN_KEY/TOKEN_PWD)"; exit 1; }
echo "token: issued"

day() { python3 -c "import datetime;print((datetime.date.today()+datetime.timedelta(days=$1)).isoformat())"; }

# shop_rq "O-D@date,O-D@date,..." -> an AirShoppingRQ in the provider's accepted shape
shop_rq() {
  python3 - "$1" <<'PY'
import os, sys
e = os.environ
program = ''
if e.get("ACCOUNT_CODE") and e.get("CARRIER"):
    program = (f'<ProgramCriteria><ProgramAccount><AccountID>{e["ACCOUNT_CODE"]}</AccountID></ProgramAccount>'
               f'<ProgramOwner><Carrier><AirlineDesigCode>{e["CARRIER"]}</AirlineDesigCode></Carrier></ProgramOwner></ProgramCriteria>')
legs = ''.join(
    f'<OriginDestCriteria><DestArrivalCriteria><IATA_LocationCode>{od.split("-")[1]}</IATA_LocationCode></DestArrivalCriteria>'
    f'<OriginDepCriteria><Date>{d}</Date><IATA_LocationCode>{od.split("-")[0]}</IATA_LocationCode></OriginDepCriteria></OriginDestCriteria>'
    for od, d in (leg.split('@') for leg in sys.argv[1].split(',')))
print(f'''<?xml version="1.0" encoding="UTF-8"?>
<IATA_AirShoppingRQ xmlns="http://www.iata.org/IATA/2015/00/2019.2/IATA_AirShoppingRQ">
<MessageDoc><RefVersionNumber>1.0</RefVersionNumber></MessageDoc>
<Party><Participant><Aggregator><AggregatorID>88888888</AggregatorID><Name>Name Aggregator</Name></Aggregator></Participant>
<Sender><TravelAgency><AgencyID>{e["AGENCY_ID"]}</AgencyID><IATA_Number>{e["IATA_NUMBER"]}</IATA_Number><Name>Agency</Name><TravelAgent><TravelAgentID>{e["AGENT_ID"]}</TravelAgentID></TravelAgent></TravelAgency></Sender></Party>
<POS><City><IATA_LocationCode>{e["POS_CITY"]}</IATA_LocationCode></City><Country><CountryCode>{e["POS_COUNTRY"]}</CountryCode></Country><RequestTime>2018-10-12T07:38:00</RequestTime></POS>
<Request><FlightCriteria>{legs}</FlightCriteria>
<Paxs><Pax><PaxID>ADT_1</PaxID><PTC>ADT</PTC></Pax></Paxs>
<ShoppingCriteria><CabinTypeCriteria><CabinTypeCode/></CabinTypeCriteria>
<ConnectionCriteria><ConnectionPrefID>CONN_1</ConnectionPrefID><MaximumConnectionQty>1</MaximumConnectionQty><StationCriteria/></ConnectionCriteria>
{program}
</ShoppingCriteria></Request></IATA_AirShoppingRQ>''')
PY
}
export AGENCY_ID IATA_NUMBER AGENT_ID ACCOUNT_CODE CARRIER POS_CITY POS_COUNTRY

call() { # label operation body-file
  local args=() h opname
  case $2 in airshopping) opname=AirShopping ;; offerprice) opname=OfferPrice ;; *) opname=$2 ;; esac
  for h in "${extra[@]}"; do
    h=$(sed 's/^ *//; s/ *$//' <<<"$h"); [ -n "$h" ] || continue
    h=${h//\{op\}/$opname}; args+=(-H "${h//\{uuid\}/$(uuidgen)}")
  done
  curl -s -m 180 -D "$t/h" -o "$t/$1.xml" -w '%{http_code}' -X POST "localhost:$PORT/ndc/$NDC_VERSION/$(path "$2")" \
    -H "Authorization: Bearer $tok" -H "X-Api-Key: $API_KEY" \
    -H 'Content-Type: application/xml' -H "Accept-Language: $LANG_CODE" \
    "${args[@]}" --data-binary @"$3" > "$t/code"
  local mode err
  mode=$(grep -i '^x-mock-mode:' "$t/h" | tr -d '\r' | awk '{print $2}')
  err=$(python3 -c "import re,sys;e=re.search(r'<Error>(.*?)</Error>',open(sys.argv[1]).read(),re.S);print(' '.join(re.findall(r'<(?:Code|DescText)>([^<]{1,120})',e.group(1))) if e else '')" "$t/$1.xml")
  printf '%-10s http %s  mode=%-18s %6d KB  offers=%-4s %s\n' "$1" "$(cat "$t/code")" "${mode:-?}" \
    $(($(wc -c <"$t/$1.xml") / 1024)) "$(grep -o '<OfferID>' "$t/$1.xml" | wc -l | tr -d ' ')" "${err:+error: $err}"
}

o=${RT%-*}; d=${RT#*-}
shop_rq "$o-$d@$(day 30),$d-$o@$(day 32)" > "$t/rt.rq"; call round-trip airshopping "$t/rt.rq"
shop_rq "$o-$d@$(day 30)" > "$t/ow.rq";                   call one-way airshopping "$t/ow.rq"
legs=""; n=30; IFS=, read -ra mc <<<"$MC"
for leg in "${mc[@]}"; do legs+="${legs:+,}$leg@$(day $n)"; n=$((n + 7)); done
shop_rq "$legs" > "$t/mc.rq";                             call multi-city airshopping "$t/mc.rq"

offer=$(grep -oE '<OfferID>[^<]+' "$t/round-trip.xml" | sed -n '1s|<OfferID>||p')
item=$(grep -oE '<OfferItemID>[^<]+' "$t/round-trip.xml" | sed -n '1s|<OfferItemID>||p')
owner=$(grep -oE '<OwnerCode>[^<]+' "$t/round-trip.xml" | sed -n '1s|<OwnerCode>||p')
if [ -n "$offer" ]; then
  # Gateways that identify the seller want the agency here as well.
  python3 - "$offer" "$item" "$owner" > "$t/op.rq" <<'PY'
import os, sys
o, i, owner = sys.argv[1:]
e = os.environ
print(f'<?xml version="1.0" encoding="UTF-8"?><IATA_OfferPriceRQ xmlns="http://www.iata.org/IATA/2015/00/2019.2/IATA_OfferPriceRQ">'
      f'<Party><Sender><TravelAgency><AgencyID>{e["AGENCY_ID"]}</AgencyID><IATA_Number>{e["IATA_NUMBER"]}</IATA_Number><Name>Agency</Name>'
      f'<TravelAgent><TravelAgentID>{e["AGENT_ID"]}</TravelAgentID></TravelAgent></TravelAgency></Sender></Party>'
      f'<Request><DataLists><PaxList><Pax><PaxID>ADT_1</PaxID><PTC>ADT</PTC></Pax></PaxList></DataLists>'
      f'<PricedOffer><SelectedOffer><OfferRefID>{o}</OfferRefID><OwnerCode>{owner}</OwnerCode>'
      f'<SelectedOfferItem><OfferItemRefID>{i}</OfferItemRefID><PaxRefID>ADT_1</PaxRefID></SelectedOfferItem>'
      f'</SelectedOffer></PricedOffer></Request></IATA_OfferPriceRQ>')
PY
  call offerPrice offerprice "$t/op.rq"
fi

echo "recorded: $(find "$REC" -name '*.xml' -newer "$t/rt.rq" | wc -l | tr -d ' ') files in capture/rs/proxy/"
if [ -n "${XSD_DIR:-}" ]; then
  for f in "$t"/round-trip.xml "$t"/one-way.xml "$t"/multi-city.xml "$t"/offerPrice.xml "$t"/op.rq; do
    [ -s "$f" ] || continue
    root=$(grep -oE '<([A-Za-z]+:)?IATA_[A-Za-z]+' "$f" | head -1 | sed 's/.*IATA_/IATA_/')
    if xmllint --noout --schema "$XSD_DIR/$root.xsd" "$f" 2>"$t/err" >/dev/null; then r=valid
    else r="INVALID ($(grep -c 'validity error' "$t/err")): $(grep -m1 -o "Element '[^']*'.\{0,80\}" "$t/err" | sed 's/{[^}]*}//g')"; fi
    echo "schema $(basename "$f" .xml): $r"
  done
fi
grep -E '^.{20}proxy' "$t/mock.log" | sed 's/^/mock: /' | tail -6
