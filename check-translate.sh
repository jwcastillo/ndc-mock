#!/usr/bin/env bash
# Verifies the translation engine on a mapping that is actually filled in.
# The scaffolds shipped in translations/ are deliberately empty, so this builds
# a temporary mapping with known renames and asserts the mechanics:
#   - namespaces are rewritten
#   - renames apply to open, close and self-closing forms
#   - a rename does NOT corrupt a longer name that starts with the same text
#   - dropped elements take their subtree with them
#   - coverage reflects how much the mapping actually accounts for
set -uo pipefail
R="$(cd "$(dirname "$0")" && pwd)"
t=$(mktemp -d); trap 'rm -rf $t' EXIT
fail() { echo "FAIL: $*"; exit 1; }

# A mapping that renames Price but must leave PriceClass alone, and drops
# TaxSummary entirely.
cat > "$R/translations/19.2-to-test.json" <<'JSON'
{
  "from": "19.2",
  "to": "test",
  "namespaces": {
    "http://www.iata.org/IATA/2015/00/2019.2/IATA_AirShoppingRS": "urn:test:airshopping"
  },
  "rename": { "Price": "FareAmount", "OfferID": "OfferIdentifier" },
  "drop": ["TaxSummary"],
  "identical": ["Response", "DataLists", "Offer", "OfferItem", "PriceClass"],
  "notes": ["temporary mapping used by check-translate.sh"]
}
JSON
cleanup() { rm -f "$R/translations/19.2-to-test.json"; }
trap 'cleanup; rm -rf $t' EXIT

cat > "$R/versions-test.json" <<'JSON'
{
  "versions": {
    "v192": { "iata": "19.2", "offerElement": "Offer", "idElements": ["OfferID","OfferItemID"], "stubs": "19.2" },
    "vtest": { "iata": "test", "offerElement": "Offer", "idElements": ["OfferIdentifier","OfferItemID"], "stubs": "test" }
  }
}
JSON

STUBS=${STUBS:-$R/stubs}
"$R/edge/ndc-edge-mock" -addr :8099 -stubs "$STUBS" -config "$R/routes.json" \
  -versions "$R/versions-test.json" -airlines "$R/airlines.json" \
  -translations "$R/translations" > "$t/server.log" 2>&1 &
srv=$!
trap 'kill $srv 2>/dev/null; cleanup; rm -f "$R/versions-test.json"; rm -rf $t' EXIT

for _ in $(seq 40); do curl -sf -o /dev/null http://localhost:8099/__health && break; sleep 0.5; done
curl -sf -o /dev/null http://localhost:8099/__health || fail "test server did not start: $(tail -3 "$t/server.log")"

# Baseline in the source generation, for comparison.
curl -sf -o "$t/src.xml" -X POST "http://localhost:8099/ndc/v192/airshopping" \
  -H 'Content-Type: application/xml' -H 'X-Mock-Offers: 3' \
  --data-binary @"$R/dataset/gru-nat-64.xml" || fail "source request failed"

code=$(curl -s -o "$t/out.xml" -D "$t/hdr.txt" -w '%{http_code}' -X POST \
  "http://localhost:8099/ndc/vtest/airshopping" -H 'Content-Type: application/xml' \
  -H 'X-Mock-Offers: 3' --data-binary @"$R/dataset/gru-nat-64.xml")
[ "$code" = 200 ] || fail "translated request returned $code"

xmllint --noout "$t/out.xml" || fail "translated document is not well-formed"

grep -qi 'x-mock-translated: 19.2->test' "$t/hdr.txt" || fail "translation header missing"

# namespace
grep -q 'urn:test:airshopping' "$t/out.xml" || fail "target namespace not applied"
grep -q '2019.2/IATA_AirShoppingRS' "$t/out.xml" && fail "source namespace survived"

# renames applied
grep -q '<FareAmount>' "$t/out.xml" || fail "Price was not renamed to FareAmount"
grep -q '<OfferIdentifier>' "$t/out.xml" || fail "OfferID was not renamed"

# the source names must be gone, in both open and close form
grep -q '<Price>' "$t/out.xml" && fail "the old <Price> element survived"
grep -q '</Price>' "$t/out.xml" && fail "the old </Price> close tag survived"

# prefix safety: PriceClass must be untouched by the Price rename
srcPC=$(grep -oE '<PriceClass>' "$t/src.xml" | wc -l | tr -d ' ')
outPC=$(grep -oE '<PriceClass>' "$t/out.xml" | wc -l | tr -d ' ')
[ "$srcPC" = "$outPC" ] || fail "PriceClass count changed from $srcPC to $outPC: a rename corrupted a longer name"
grep -q 'FareAmountClass' "$t/out.xml" && fail "the Price rename bled into PriceClass"

# drop took the subtree
grep -q '<TaxSummary>' "$t/out.xml" && fail "dropped element survived"
grep -q '</TaxSummary>' "$t/out.xml" && fail "dropped element left a dangling close tag"

# coverage must be meaningfully above the empty-scaffold case
cov=$(grep -i 'x-mock-translation-coverage' "$t/hdr.txt" | tr -dc '0-9')
[ -n "$cov" ] || fail "coverage header missing"
[ "$cov" -gt 0 ] || fail "a filled mapping still reports 0% coverage"

echo "OK: namespace rewritten, renames applied, PriceClass intact ($outPC), subtree dropped, coverage ${cov}%"
