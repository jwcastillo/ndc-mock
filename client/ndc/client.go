// Package ndc is a small client for an NDC endpoint, shared by the CLI and the
// MCP server. It asks for the JSON projection rather than NDC XML: an agent or
// a terminal has no use for a multi-megabyte document, and the projection
// carries what a caller actually acts on.
package ndc

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	defaultTimeout = 60 * time.Second
	// A projection for a human or an agent is small; anything larger means the
	// endpoint answered with something unexpected.
	maxResponse = 8 << 20
	// Offers are capped by default because an agent pays for every token it
	// reads, and a shopping response can carry hundreds.
	DefaultMaxOffers = 10
)

type Client struct {
	BaseURL string
	Version string
	HTTP    *http.Client
	// Headers are forwarded verbatim, for endpoints that require credentials.
	// They are never logged.
	Headers map[string]string
}

func New(baseURL, version string) *Client {
	if version == "" {
		version = "v192"
	}
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Version: version,
		HTTP:    &http.Client{Timeout: defaultTimeout},
	}
}

// --- input validation -------------------------------------------------------
//
// Requests are assembled as XML, so every value that reaches the document is
// checked against its expected shape. Rejecting bad input is cheaper and safer
// than escaping it, and it catches typos before a request goes out.

var (
	reAirport = regexp.MustCompile(`^[A-Z]{3}$`)
	reDate    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reCarrier = regexp.MustCompile(`^[A-Z0-9]{2}$`)
)

type SearchParams struct {
	Origin     string
	Dest       string
	DepartDate string
	ReturnDate string
	ADT        int
	CHD        int
	INF        int
	Airline    string
	MaxOffers  int
	Cabin      string
}

func (p *SearchParams) validate() error {
	p.Origin, p.Dest = strings.ToUpper(p.Origin), strings.ToUpper(p.Dest)
	p.Airline = strings.ToUpper(p.Airline)
	switch {
	case !reAirport.MatchString(p.Origin):
		return fmt.Errorf("origin %q is not a three-letter airport code", p.Origin)
	case !reAirport.MatchString(p.Dest):
		return fmt.Errorf("destination %q is not a three-letter airport code", p.Dest)
	case p.Origin == p.Dest:
		return fmt.Errorf("origin and destination are both %s", p.Origin)
	case !reDate.MatchString(p.DepartDate):
		return fmt.Errorf("departure date %q is not YYYY-MM-DD", p.DepartDate)
	case p.ReturnDate != "" && !reDate.MatchString(p.ReturnDate):
		return fmt.Errorf("return date %q is not YYYY-MM-DD", p.ReturnDate)
	case p.ReturnDate != "" && p.ReturnDate < p.DepartDate:
		return fmt.Errorf("return date %s precedes departure %s", p.ReturnDate, p.DepartDate)
	case p.Airline != "" && !reCarrier.MatchString(p.Airline):
		return fmt.Errorf("carrier %q is not a two-character code", p.Airline)
	}
	if p.ADT == 0 && p.CHD == 0 && p.INF == 0 {
		p.ADT = 1
	}
	if p.ADT < 0 || p.CHD < 0 || p.INF < 0 {
		return fmt.Errorf("passenger counts cannot be negative")
	}
	if total := p.ADT + p.CHD + p.INF; total > 9 {
		return fmt.Errorf("%d passengers exceeds the usual limit of 9", total)
	}
	if p.INF > p.ADT {
		return fmt.Errorf("%d infants need at least as many adults, got %d", p.INF, p.ADT)
	}
	return nil
}

// validateID guards identifiers that are echoed into a request document.
func validateID(kind, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", kind)
	}
	if len(v) > 4096 {
		return fmt.Errorf("%s is implausibly long (%d characters)", kind, len(v))
	}
	if strings.ContainsAny(v, "<>&\"'") {
		return fmt.Errorf("%s contains markup characters", kind)
	}
	return nil
}

// --- response shapes --------------------------------------------------------

type Price struct {
	Currency string `json:"currency"`
	Total    int    `json:"total"`
	Base     int    `json:"base,omitempty"`
	Taxes    int    `json:"taxes,omitempty"`
}

type Segment struct {
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	Carrier     string `json:"carrier"`
	FlightNum   string `json:"flightNumber"`
	Departure   string `json:"departure"`
	Arrival     string `json:"arrival"`
}

type Offer struct {
	OfferID   string    `json:"offerId"`
	Price     Price     `json:"price"`
	Segments  []Segment `json:"segments"`
	RoundTrip bool      `json:"roundTrip"`
}

type Shopping struct {
	ShoppingResponseID string  `json:"shoppingResponseId"`
	Origin             string  `json:"origin"`
	Destination        string  `json:"destination"`
	DepartureDate      string  `json:"departureDate"`
	ReturnDate         string  `json:"returnDate,omitempty"`
	Currency           string  `json:"currency"`
	OfferCount         int     `json:"offerCount"`
	Offers             []Offer `json:"offers"`
	Truncated          bool    `json:"truncated,omitempty"`
}

type Order struct {
	OrderID  string   `json:"orderId"`
	Status   string   `json:"status"`
	Currency string   `json:"currency"`
	Total    int      `json:"total"`
	Offer    Offer    `json:"offer"`
	History  []string `json:"history"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- operations -------------------------------------------------------------

func (c *Client) Search(ctx context.Context, p SearchParams) (*Shopping, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	body := buildAirShopping(p)
	hdr := map[string]string{}
	if p.Airline != "" {
		hdr["X-Mock-Airline"] = p.Airline
	}
	var out Shopping
	if err := c.do(ctx, "airshopping", body, hdr, &out); err != nil {
		return nil, err
	}
	// Truncation happens here rather than at the endpoint: the caller asked for
	// a readable answer, and the endpoint is free to serve the full set.
	max := p.MaxOffers
	if max <= 0 {
		max = DefaultMaxOffers
	}
	if len(out.Offers) > max {
		out.Offers = out.Offers[:max]
		out.Truncated = true
	}
	return &out, nil
}

func (c *Client) Price(ctx context.Context, offerID string) (map[string]any, error) {
	if err := validateID("offer id", offerID); err != nil {
		return nil, err
	}
	var out map[string]any
	err := c.do(ctx, "offerprice", wrapID("IATA_OfferPriceRQ", "OfferID", offerID), nil, &out)
	return out, err
}

func (c *Client) CreateOrder(ctx context.Context, offerID string) (*Order, error) {
	if err := validateID("offer id", offerID); err != nil {
		return nil, err
	}
	var out Order
	err := c.do(ctx, "ordercreate", wrapID("IATA_OrderCreateRQ", "OfferID", offerID), nil, &out)
	return &out, err
}

func (c *Client) RetrieveOrder(ctx context.Context, orderID string) (*Order, error) {
	return c.orderOp(ctx, "orderretrieve", orderID)
}

func (c *Client) CancelOrder(ctx context.Context, orderID string) (*Order, error) {
	return c.orderOp(ctx, "ordercancel", orderID)
}

func (c *Client) orderOp(ctx context.Context, op, orderID string) (*Order, error) {
	if err := validateID("order id", orderID); err != nil {
		return nil, err
	}
	var out Order
	err := c.do(ctx, op, wrapID("IATA_OrderRetrieveRQ", "OrderID", orderID), nil, &out)
	return &out, err
}

func (c *Client) Services(ctx context.Context, offerID string, count int) (map[string]any, error) {
	return c.sized(ctx, "servicelist", offerID, count)
}

func (c *Client) Seats(ctx context.Context, offerID string, rows int) (map[string]any, error) {
	return c.sized(ctx, "seatavailability", offerID, rows)
}

func (c *Client) sized(ctx context.Context, op, offerID string, n int) (map[string]any, error) {
	if err := validateID("offer id", offerID); err != nil {
		return nil, err
	}
	hdr := map[string]string{}
	if n > 0 {
		hdr["X-Mock-Offers"] = fmt.Sprint(n)
	}
	var out map[string]any
	err := c.do(ctx, op, wrapID("IATA_ServiceListRQ", "OfferID", offerID), hdr, &out)
	return out, err
}

// --- transport --------------------------------------------------------------

func (c *Client) do(ctx context.Context, op string, body []byte, extra map[string]string, out any) error {
	url := fmt.Sprintf("%s/ndc/%s/%s?format=json", c.BaseURL, c.Version, op)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("%s: reading response: %w", op, err)
	}
	if resp.StatusCode >= 400 {
		var e apiError
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s: %s", op, e.Message)
		}
		return fmt.Errorf("%s: endpoint returned %d", op, resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: response was not the JSON projection (is -format supported?): %w", op, err)
	}
	return nil
}

// --- request documents ------------------------------------------------------

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func wrapID(root, elem, id string) []byte {
	return fmt.Appendf(nil,
		`<?xml version="1.0" encoding="UTF-8"?><%s><Request><%s>%s</%s></Request></%s>`,
		root, elem, esc(id), elem, root)
}

func buildAirShopping(p SearchParams) []byte {
	var b bytes.Buffer
	fmt.Fprint(&b, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprint(&b, `<IATA_AirShoppingRQ xmlns="http://www.iata.org/IATA/2015/00/2019.2/IATA_AirShoppingRQ">`)
	fmt.Fprint(&b, `<MessageDoc><RefVersionNumber>1.0</RefVersionNumber></MessageDoc>`)
	fmt.Fprint(&b, `<Party><Participant><Aggregator><AggregatorID>88888888</AggregatorID><Name>Aggregator</Name></Aggregator></Participant>`)
	fmt.Fprint(&b, `<Sender><TravelAgency><AgencyID>00000000-0000-0000-0000-000000000000</AgencyID>`)
	fmt.Fprint(&b, `<IATA_Number>00000000</IATA_Number><Name>Agency</Name>`)
	fmt.Fprint(&b, `<TravelAgent><TravelAgentID>agent@example.com</TravelAgentID></TravelAgent></TravelAgency></Sender></Party>`)
	fmt.Fprint(&b, `<POS><City><IATA_LocationCode>`+esc(p.Origin)+`</IATA_LocationCode></City>`)
	fmt.Fprint(&b, `<Country><CountryCode>US</CountryCode></Country><RequestTime>2018-10-12T07:38:00</RequestTime></POS>`)
	fmt.Fprint(&b, `<Request><FlightCriteria>`)
	leg(&b, p.Origin, p.Dest, p.DepartDate)
	if p.ReturnDate != "" {
		leg(&b, p.Dest, p.Origin, p.ReturnDate)
	}
	fmt.Fprint(&b, `</FlightCriteria><Paxs>`)
	paxBlock(&b, "ADT", p.ADT)
	paxBlock(&b, "CHD", p.CHD)
	paxBlock(&b, "INF", p.INF)
	fmt.Fprint(&b, `</Paxs><ShoppingCriteria><CabinTypeCriteria><CabinTypeCode>`+esc(p.Cabin)+`</CabinTypeCode></CabinTypeCriteria>`)
	fmt.Fprint(&b, `<ConnectionCriteria><ConnectionPrefID>CONN_1</ConnectionPrefID><MaximumConnectionQty>1</MaximumConnectionQty><StationCriteria/></ConnectionCriteria>`)
	fmt.Fprint(&b, `</ShoppingCriteria></Request></IATA_AirShoppingRQ>`)
	return b.Bytes()
}

func leg(b *bytes.Buffer, from, to, date string) {
	fmt.Fprint(b, `<OriginDestCriteria><DestArrivalCriteria><IATA_LocationCode>`+esc(to)+`</IATA_LocationCode></DestArrivalCriteria>`)
	fmt.Fprint(b, `<OriginDepCriteria><Date>`+esc(date)+`</Date><IATA_LocationCode>`+esc(from)+`</IATA_LocationCode></OriginDepCriteria></OriginDestCriteria>`)
}

func paxBlock(b *bytes.Buffer, ptc string, n int) {
	for i := 1; i <= n; i++ {
		fmt.Fprintf(b, `<Pax><PaxID>%s_%d</PaxID><PTC>%s</PTC></Pax>`, ptc, i, ptc)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
