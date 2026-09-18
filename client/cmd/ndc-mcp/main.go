// Command ndc-mcp exposes an NDC endpoint as MCP tools over stdio, so an agent
// can shop, price and order without knowing anything about IATA XML.
//
// The protocol surface an agent needs is small - initialize, tools/list,
// tools/call - so this speaks JSON-RPC directly rather than taking on an SDK
// dependency that would have to be tracked as the spec moves.
//
// Every tool answers with the JSON projection, never NDC XML: a shopping
// document runs to several megabytes and an agent pays for every token it reads.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"ndcmock/client/ndc"
)

const protocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var client *ndc.Client

func main() {
	base := flag.String("url", envOr("NDC_URL", "http://localhost:8090"), "NDC endpoint")
	version := flag.String("version", envOr("NDC_VERSION", "v192"), "NDC version segment")
	timeout := flag.Duration("timeout", 60*time.Second, "request timeout")
	flag.Parse()

	client = ndc.New(*base, *version)
	client.HTTP.Timeout = *timeout
	if v := os.Getenv("NDC_AUTH"); v != "" {
		client.Headers = map[string]string{"Authorization": v}
	}
	if v := os.Getenv("NDC_API_KEY"); v != "" {
		if client.Headers == nil {
			client.Headers = map[string]string{}
		}
		client.Headers["X-Api-Key"] = v
	}

	// stdout carries the protocol, so diagnostics go to stderr. Writing a stray
	// line to stdout corrupts the stream.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			write(out, rpcResponse{JSONRPC: "2.0", Error: &rpcError{-32700, "parse error"}})
			continue
		}
		resp := dispatch(req)
		// A notification carries no id and takes no reply.
		if resp == nil {
			continue
		}
		write(out, *resp)
	}
	if err := in.Err(); err != nil {
		log.Printf("stdin: %v", err)
	}
}

func write(w *bufio.Writer, r rpcResponse) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	w.Write(b)
	w.WriteByte('\n')
	w.Flush()
}

func dispatch(req rpcRequest) *rpcResponse {
	if len(req.ID) == 0 {
		return nil // notification
	}
	ok := func(v any) *rpcResponse {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: v}
	}
	switch req.Method {
	case "initialize":
		return ok(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "ndc-mcp", "version": "1.0.0"},
		})
	case "tools/list":
		return ok(map[string]any{"tools": tools()})
	case "tools/call":
		return callTool(req)
	case "ping":
		return ok(map[string]any{})
	default:
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{-32601, "unknown method " + req.Method}}
	}
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func num(m map[string]any, k string, def int) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return def
}

func callTool(req rpcRequest) *rpcResponse {
	var p struct {
		Name string         `json:"name"`
		Args map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{-32602, "invalid params"}}
	}
	if p.Args == nil {
		p.Args = map[string]any{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), client.HTTP.Timeout)
	defer cancel()

	var result any
	var err error
	switch p.Name {
	case "ndc_search":
		result, err = client.Search(ctx, ndc.SearchParams{
			Origin: str(p.Args, "origin"), Dest: str(p.Args, "destination"),
			DepartDate: str(p.Args, "departureDate"), ReturnDate: str(p.Args, "returnDate"),
			ADT: num(p.Args, "adults", 1), CHD: num(p.Args, "children", 0),
			INF: num(p.Args, "infants", 0), Airline: str(p.Args, "airline"),
			MaxOffers: num(p.Args, "maxOffers", ndc.DefaultMaxOffers),
		})
	case "ndc_price":
		result, err = client.Price(ctx, str(p.Args, "offerId"))
	case "ndc_order_create":
		result, err = client.CreateOrder(ctx, str(p.Args, "offerId"))
	case "ndc_order_retrieve":
		result, err = client.RetrieveOrder(ctx, str(p.Args, "orderId"))
	case "ndc_order_cancel":
		result, err = client.CancelOrder(ctx, str(p.Args, "orderId"))
	case "ndc_services":
		result, err = client.Services(ctx, str(p.Args, "offerId"), num(p.Args, "count", 0))
	case "ndc_seats":
		result, err = client.Seats(ctx, str(p.Args, "offerId"), num(p.Args, "rows", 0))
	default:
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{-32602, "unknown tool " + p.Name}}
	}

	// A failed call is reported as tool content with isError, not as a protocol
	// error: the agent should see what went wrong and be able to retry.
	if err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": err.Error()}},
			"isError": true,
		}}
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(b)}},
	}}
}

func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func sprop(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func iprop(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func tools() []toolDef {
	return []toolDef{
		{
			Name: "ndc_search",
			Description: "Search flight offers for a route and date. Returns a capped list of " +
				"offers with prices and routings. Use the returned offerId with the other tools.",
			InputSchema: obj(map[string]any{
				"origin":        sprop("Origin airport code, e.g. LIM"),
				"destination":   sprop("Destination airport code, e.g. SCL"),
				"departureDate": sprop("Departure date, YYYY-MM-DD"),
				"returnDate":    sprop("Return date for a round trip, YYYY-MM-DD"),
				"adults":        iprop("Adult passengers, default 1"),
				"children":      iprop("Child passengers"),
				"infants":       iprop("Infant passengers, cannot exceed adults"),
				"airline":       sprop("Two-letter carrier code to present offers as"),
				"maxOffers":     iprop("How many offers to return, default 10"),
			}, "origin", "destination", "departureDate"),
		},
		{
			Name:        "ndc_price",
			Description: "Confirm the firm price of an offer returned by ndc_search.",
			InputSchema: obj(map[string]any{"offerId": sprop("Offer id from ndc_search")}, "offerId"),
		},
		{
			Name: "ndc_order_create",
			Description: "Create an order from a priced offer. This is the booking step and " +
				"changes state; confirm with the user before calling it.",
			InputSchema: obj(map[string]any{"offerId": sprop("Offer id to book")}, "offerId"),
		},
		{
			Name:        "ndc_order_retrieve",
			Description: "Look up an existing order and its current status.",
			InputSchema: obj(map[string]any{"orderId": sprop("Order id from ndc_order_create")}, "orderId"),
		},
		{
			Name: "ndc_order_cancel",
			Description: "Cancel an order. This changes state and is not reversible; confirm " +
				"with the user before calling it.",
			InputSchema: obj(map[string]any{"orderId": sprop("Order id to cancel")}, "orderId"),
		},
		{
			Name:        "ndc_services",
			Description: "List ancillary services available for an offer, such as bags and meals.",
			InputSchema: obj(map[string]any{
				"offerId": sprop("Offer id"),
				"count":   iprop("How many services to return"),
			}, "offerId"),
		},
		{
			Name:        "ndc_seats",
			Description: "Return the seat map for an offer, with availability and prices.",
			InputSchema: obj(map[string]any{
				"offerId": sprop("Offer id"),
				"rows":    iprop("How many rows to return"),
			}, "offerId"),
		},
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
