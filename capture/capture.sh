#!/usr/bin/env bash
# Records a real AirShopping response. Read-only. Writes to capture/rs/.
# Provider URL, credentials and agency identity come from the environment.
# Usage: ./capture.sh GRU NAT 2026-09-30 [2026-10-02]
set -uo pipefail
O=$1 D=$2 DEP=$3 RET=${4:-}
set -a; : "${NDC_BASE_URL:?set the provider base URL}" "${TOKEN_KEY:?set it}" "${TOKEN_PWD:?set it}" "${API_KEY:?set it}"
: "${NDC_VERSION:=v192}" "${AGENCY_ID:=00000000-0000-0000-0000-000000000000}"
: "${IATA_NUMBER:=00000000}" "${AGENT_ID:=agent@example.com}" "${ACCOUNT_CODE:=ACCOUNT}"
: "${POS_CITY:?set the point-of-sale city}" "${POS_COUNTRY:?set the point-of-sale country}" "${LANG:=en}"
: "${TOKEN_PATH:=/oauth/token}" "${CARRIER:=}"; set +a
mkdir -p "$(dirname "$0")/rs"
tokresp=$(curl -s -m 30 -w $'\n%{http_code}' -X POST "${NDC_BASE_URL}${TOKEN_PATH}" \
  -H "x-api-key: ${TOKEN_KEY}" -H "Authorization: ${TOKEN_PWD}" \
  -H 'Content-Type: application/x-www-form-urlencoded' -d 'grant_type=client_credentials')
tokcode=$(tail -1 <<<"$tokresp"); tokbody=$(sed '$d' <<<"$tokresp")
TOK=$(python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token",""))' <<<"$tokbody" 2>/dev/null)
if [ -z "$TOK" ]; then
  echo "token: the provider did not issue one (http $tokcode): $(head -c 160 <<<"$tokbody" | tr -d '\n')"
  # chequeo de forma sin exponer valores
  exit 1
fi

# Some gateways require the fare programme the agency sells under.
program=""
[ -n "$CARRIER" ] && [ "$ACCOUNT_CODE" != ACCOUNT ] && program="<ProgramCriteria><ProgramAccount><AccountID>$ACCOUNT_CODE</AccountID></ProgramAccount><ProgramOwner><Carrier><AirlineDesigCode>$CARRIER</AirlineDesigCode></Carrier></ProgramOwner></ProgramCriteria>"

vuelta=""
[ -n "$RET" ] && vuelta="<OriginDestCriteria><DestArrivalCriteria><IATA_LocationCode>$O</IATA_LocationCode></DestArrivalCriteria><OriginDepCriteria><Date>$RET</Date><IATA_LocationCode>$D</IATA_LocationCode></OriginDepCriteria></OriginDestCriteria>"

rq=$(cat <<XML
<?xml version="1.0" encoding="UTF-8"?>
<IATA_AirShoppingRQ xmlns="http://www.iata.org/IATA/2015/00/2019.2/IATA_AirShoppingRQ">
<MessageDoc><RefVersionNumber>1.0</RefVersionNumber></MessageDoc>
<Party><Participant><Aggregator><AggregatorID>88888888</AggregatorID><Name>Name Aggregator</Name></Aggregator></Participant>
<Sender><TravelAgency><AgencyID>$AGENCY_ID</AgencyID><IATA_Number>$IATA_NUMBER</IATA_Number><Name>Agency</Name><TravelAgent><TravelAgentID>$AGENT_ID</TravelAgentID></TravelAgent></TravelAgency></Sender></Party>
<POS><City><IATA_LocationCode>$POS_CITY</IATA_LocationCode></City><Country><CountryCode>$POS_COUNTRY</CountryCode></Country><RequestTime>2018-10-12T07:38:00</RequestTime></POS>
<Request><FlightCriteria>
<OriginDestCriteria><DestArrivalCriteria><IATA_LocationCode>$D</IATA_LocationCode></DestArrivalCriteria><OriginDepCriteria><Date>$DEP</Date><IATA_LocationCode>$O</IATA_LocationCode></OriginDepCriteria></OriginDestCriteria>
$vuelta
</FlightCriteria>
<Paxs><Pax><PaxID>ADT_1</PaxID><PTC>ADT</PTC></Pax></Paxs>
<ShoppingCriteria><CabinTypeCriteria><CabinTypeCode/></CabinTypeCriteria>
<ConnectionCriteria><ConnectionPrefID>CONN_1</ConnectionPrefID><MaximumConnectionQty>1</MaximumConnectionQty><StationCriteria/></ConnectionCriteria>
<ConnectionCriteria><ConnectionPrefID>CONN_2</ConnectionPrefID><MaximumConnectionQty>0</MaximumConnectionQty><StationCriteria/></ConnectionCriteria>
$program
</ShoppingCriteria></Request></IATA_AirShoppingRQ>
XML
)
out="$(dirname "$0")/rs/airshopping-${O}-${D}.xml"
code=$(curl -s -m 120 -o "$out" -w '%{http_code}' -X POST "${NDC_BASE_URL}/ndc/${NDC_VERSION}/airshopping" \
  -H "Authorization: Bearer $TOK" -H "X-Api-Key: ${API_KEY}" \
  -H 'Content-Type: application/xml' -H "X-Country: ${POS_COUNTRY}" -H "Accept-Language: ${LANG}" \
  -H "X-Track-Id: $(uuidgen)" \
  --data-binary "$rq")
sz=$(wc -c <"$out" | tr -d ' ')
echo "$O-$D: http $code, $((sz/1024)) KB -> $out"
