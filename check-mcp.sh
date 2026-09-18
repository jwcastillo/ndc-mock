#!/usr/bin/env bash
# Drives the MCP server over stdio the way a client does: initialize, the
# initialized notification, tools/list, then tool calls.
#
# What this does NOT cover: a real MCP client. This speaks the protocol
# directly, so a pass means the server answers correctly, not that Claude
# Desktop or another host is happy with it.
set -uo pipefail
R="$(cd "$(dirname "$0")" && pwd)"
URL=${URL:-http://localhost:8090}
t=$(mktemp -d); trap 'rm -rf $t' EXIT
fail() { echo "FAIL: $*"; exit 1; }

[ -x "$R/bin/ndc-mcp" ] || fail "bin/ndc-mcp is not built (run: make build)"
curl -sf -o /dev/null "$URL/__health" || fail "no NDC endpoint at $URL (start it with: make edge)"

# One session, several requests: a client keeps the process alive across calls.
{
  echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"check-mcp","version":"1"}}}'
  echo '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  echo '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ndc_search","arguments":{"origin":"GRU","destination":"NAT","departureDate":"2026-09-30","returnDate":"2026-10-02","maxOffers":3}}}'
  echo '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"ndc_search","arguments":{"origin":"ZZ","destination":"NAT","departureDate":"2026-09-30"}}}'
  echo '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}'
  echo 'this is not json'
  echo '{"jsonrpc":"2.0","id":6,"method":"ping"}'
} | "$R/bin/ndc-mcp" -url "$URL" > "$t/out.jsonl" 2>"$t/err.log"

python3 - "$t/out.jsonl" <<'PY' || exit 1
import json, sys

lines = [l for l in open(sys.argv[1]) if l.strip()]
by_id, errors = {}, []
for l in lines:
    d = json.loads(l)
    if d.get("id") is not None:
        by_id[d["id"]] = d
    else:
        errors.append(d)

def die(msg):
    print("FAIL:", msg); sys.exit(1)

# A notification must not be answered.
if any(d.get("id") == "notifications/initialized" for d in lines if isinstance(d, dict)):
    die("the initialized notification drew a reply")

r = by_id.get(1, {}).get("result") or die("initialize did not return a result")
if not r.get("protocolVersion"):
    die("initialize returned no protocolVersion")
if "tools" not in (r.get("capabilities") or {}):
    die("server did not advertise the tools capability")

tools = (by_id.get(2, {}).get("result") or {}).get("tools")
if not tools:
    die("tools/list returned nothing")
names = {t["name"] for t in tools}
expected = {"ndc_search", "ndc_price", "ndc_order_create", "ndc_order_retrieve",
            "ndc_order_cancel", "ndc_services", "ndc_seats"}
missing = expected - names
if missing:
    die(f"tools missing: {sorted(missing)}")
for t in tools:
    if not t.get("description"):
        die(f"{t['name']} has no description; an agent picks tools by description")
    s = t.get("inputSchema") or {}
    if s.get("type") != "object" or "properties" not in s:
        die(f"{t['name']} has no usable inputSchema")
# The tools that change state have to say so, or an agent will call them freely.
for n in ("ndc_order_create", "ndc_order_cancel"):
    d = next(t["description"].lower() for t in tools if t["name"] == n)
    if "confirm" not in d and "state" not in d:
        die(f"{n} does not warn that it changes state")

call = (by_id.get(3, {}).get("result") or {})
if call.get("isError"):
    die(f"ndc_search failed: {call['content'][0]['text'][:120]}")
payload = json.loads(call["content"][0]["text"])
if payload.get("offerCount", 0) <= 0:
    die("ndc_search returned no offers")
if len(payload.get("offers", [])) > 3:
    die("maxOffers was not honoured; an agent pays for every token it reads")
if not payload["offers"][0].get("offerId"):
    die("offers carry no identifier, so the flow cannot be chained")

bad = (by_id.get(4, {}).get("result") or {})
if not bad.get("isError"):
    die("invalid input was not reported as a tool error")
if by_id.get(4, {}).get("error"):
    die("a tool failure came back as a protocol error; an agent cannot retry that")

if not (by_id.get(5, {}) or {}).get("error"):
    die("an unknown tool should be a protocol error")

if "result" not in by_id.get(6, {}):
    die("the server stopped responding after a malformed line")

print(f"OK: initialize + {len(tools)} tools with schemas, maxOffers honoured, "
      "tool errors are content not protocol, malformed input survived")
PY
