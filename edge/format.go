package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The mock answers either NDC XML or a reduced JSON projection, the way a BFF
// in front of an NDC provider would. Selection is by Accept header or a
// ?format= query parameter, with XML as the default.
//
// The JSON projection is built from decoded offer identifiers rather than by
// converting the XML. A BFF returns far less than NDC does, and parsing a
// multi-megabyte document per request would dominate the response time.
type wireFormat int

const (
	formatXML wireFormat = iota
	formatJSON
)

func formatOf(r *http.Request) wireFormat {
	if f := strings.ToLower(r.URL.Query().Get("format")); f != "" {
		if f == "json" {
			return formatJSON
		}
		return formatXML
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") {
		return formatJSON
	}
	return formatXML
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeXML(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/xml;charset=UTF-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// --- JSON projections -------------------------------------------------------

type jsonPrice struct {
	Currency string `json:"currency"`
	Total    int    `json:"total"`
	Base     int    `json:"base,omitempty"`
	Taxes    int    `json:"taxes,omitempty"`
}

type jsonSegment struct {
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	Carrier     string `json:"carrier"`
	FlightNum   string `json:"flightNumber"`
	Departure   string `json:"departure"`
	Arrival     string `json:"arrival"`
}

type jsonPax struct {
	ADT int `json:"adt"`
	CHD int `json:"chd"`
	INF int `json:"inf"`
}

type jsonOffer struct {
	OfferID     string        `json:"offerId"`
	OfferItemID string        `json:"offerItemId,omitempty"`
	Price       jsonPrice     `json:"price"`
	Segments    []jsonSegment `json:"segments"`
	Passengers  jsonPax       `json:"passengers"`
	RoundTrip   bool          `json:"roundTrip"`
}

type jsonShopping struct {
	ShoppingResponseID string      `json:"shoppingResponseId"`
	Origin             string      `json:"origin"`
	Destination        string      `json:"destination"`
	DepartureDate      string      `json:"departureDate"`
	ReturnDate         string      `json:"returnDate,omitempty"`
	Legs               []jsonLeg   `json:"legs"`
	Currency           string      `json:"currency"`
	OfferCount         int         `json:"offerCount"`
	Offers             []jsonOffer `json:"offers"`
}

type jsonLeg struct {
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	Date        string `json:"date"`
}

type jsonError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Owner   string `json:"owner"`
}

// offerToJSON projects one decoded identifier into the BFF shape.
func offerToJSON(id string, info offerInfo) jsonOffer {
	var segs []jsonSegment
	for _, l := range info.legs() {
		segs = append(segs, jsonSegment{
			Origin: l.from, Destination: l.to, Carrier: info.Carrier, FlightNum: l.flight,
			Departure: l.dep, Arrival: l.arr,
		})
	}
	// Tax split is illustrative; real providers break it out per tax code.
	taxes := info.Amount * 18 / 100
	return jsonOffer{
		OfferID: id,
		Price: jsonPrice{
			Currency: info.Currency, Total: info.Amount,
			Base: info.Amount - taxes, Taxes: taxes,
		},
		Segments:   segs,
		Passengers: jsonPax{ADT: info.ADT, CHD: info.CHD, INF: info.INF},
		RoundTrip:  info.RoundTrip,
	}
}
