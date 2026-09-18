package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// query is the only thing the mock needs out of the AirShoppingRQ for the
// response to agree with what was asked. Origin, Dest, DepDate and RetDate
// describe the first leg and a return; Legs holds every leg.
type query struct {
	Origin, Dest     string
	DepDate, RetDate string
	Legs             []leg
	ADT, CHD, INF    int
	Country, Lang    string
}

// leg is one origin-destination of an itinerary.
type leg struct{ From, To, Date string }

// pattern is an itinerary's shape with the airports numbered by first
// appearance: "0-1" one way, "0-1,1-0" a return, "0-1,1-2,2-0" a three-leg
// circuit. A reference can only be rewritten onto a request of the same shape.
func pattern(legs []leg) string {
	idx := map[string]int{}
	var parts []string
	for _, l := range legs {
		for _, a := range []string{l.From, l.To} {
			if _, ok := idx[a]; !ok {
				idx[a] = len(idx)
			}
		}
		parts = append(parts, strconv.Itoa(idx[l.From])+"-"+strconv.Itoa(idx[l.To]))
	}
	return strings.Join(parts, ",")
}

// airports lists the distinct airports of an itinerary in order of appearance.
func airports(legs []leg) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range legs {
		for _, a := range []string{l.From, l.To} {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out
}

// multiCity reports an itinerary that is neither one way nor a return.
func (q query) multiCity() bool {
	p := pattern(q.Legs)
	return p != "0-1" && p != "0-1,1-0"
}

func (q query) retOr(d string) string {
	if q.RetDate != "" {
		return q.RetDate
	}
	return d
}

func (q query) paxSeg() string { return fmt.Sprintf("PX=%d/%d/%d", q.ADT, q.CHD, q.INF) }

type originDest struct {
	Dep struct {
		Date string `xml:"Date"`
		Loc  string `xml:"IATA_LocationCode"`
	} `xml:"OriginDepCriteria"`
	Arr struct {
		Loc string `xml:"IATA_LocationCode"`
	} `xml:"DestArrivalCriteria"`
}

type paxPTC struct {
	PTC string `xml:"PTC"`
}

// Only the fields that are used; the rest of the request is ignored on purpose.
// Both generations are read: 21.3 wrapped the same criteria in FlightRequest and
// renamed Paxs to PaxList, keeping the leaves. Tags carry no namespace, so they
// match 19.2's per-message namespace and 21.3's common-types one alike.
type airShoppingRQ struct {
	POS struct {
		Country struct {
			CountryCode string `xml:"CountryCode"`
		} `xml:"Country"`
	} `xml:"POS"`
	Request struct {
		OD192    []originDest `xml:"FlightCriteria>OriginDestCriteria"`
		OD213    []originDest `xml:"FlightRequest>FlightRequestOriginDestinationsCriteria>OriginDestCriteria"`
		Specific []struct{}   `xml:"FlightRequest>FlightRequestSpecificOriginDestinations"`
		Affinity []struct{}   `xml:"FlightRequest>AffinityShoppingCriteria"`
		Pax192   []paxPTC     `xml:"Paxs>Pax"`
		Pax213   []paxPTC     `xml:"PaxList>Pax"`
	} `xml:"Request"`
}

// errMultiCity marks a multi-city or open-jaw itinerary for which no reference
// of the same shape is loaded. Answering it from a reference of another shape
// would return a trip that was not asked for.
var errMultiCity = errors.New("no multi-city reference of this itinerary's shape is loaded")

func parseQuery(body []byte) (query, error) {
	var rq airShoppingRQ
	if err := xml.Unmarshal(body, &rq); err != nil {
		return query{}, fmt.Errorf("unreadable AirShoppingRQ: %w", err)
	}
	if len(rq.Request.Specific) > 0 || len(rq.Request.Affinity) > 0 {
		return query{}, fmt.Errorf("only FlightRequestOriginDestinationsCriteria is supported, not specific or affinity shopping")
	}
	od := append(rq.Request.OD192, rq.Request.OD213...)
	if len(od) == 0 {
		return query{}, fmt.Errorf("request carries no OriginDestCriteria")
	}
	q := query{
		Origin:  strings.ToUpper(od[0].Dep.Loc),
		Dest:    strings.ToUpper(od[0].Arr.Loc),
		DepDate: od[0].Dep.Date,
		Country: rq.POS.Country.CountryCode,
	}
	for _, o := range od {
		q.Legs = append(q.Legs, leg{strings.ToUpper(o.Dep.Loc), strings.ToUpper(o.Arr.Loc), o.Dep.Date})
	}
	if pattern(q.Legs) == "0-1,1-0" {
		q.RetDate = od[1].Dep.Date
	}
	for _, p := range append(rq.Request.Pax192, rq.Request.Pax213...) {
		switch strings.ToUpper(p.PTC) {
		case "ADT":
			q.ADT++
		case "CHD":
			q.CHD++
		case "INF", "INS":
			q.INF++
		default:
			q.ADT++
		}
	}
	if q.Origin == "" || q.Dest == "" || q.DepDate == "" {
		return query{}, fmt.Errorf("missing origin, destination or departure date")
	}
	if q.ADT+q.CHD+q.INF == 0 {
		return query{}, fmt.Errorf("request carries no passengers")
	}
	return q, nil
}
