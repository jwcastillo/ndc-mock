package main

import (
	"regexp"
	"strconv"
	"strings"
)

// offerInfo is what an offer identifier tells us about the trip it represents.
// Because the identifier is self-describing, the operations that follow
// AirShopping can build a coherent response from the identifier alone, with no
// reference response of their own.
type offerInfo struct {
	Origin, Dest     string
	DepDate, RetDate string
	RoundTrip        bool
	Legs             []leg // every leg; Origin..RetDate mirror the first and a return
	Carrier          string
	FlightNum        string
	DepTime, ArrTime string
	Currency         string
	Amount           int
	ADT, CHD, INF    int
	Raw              string
}

var (
	reInfoTrip = reTrip // the same trip segment the template reads
	reInfoFlt  = regexp.MustCompile(`^IF=([A-Z]{3}),([A-Z]{3}),(\d+),([A-Z0-9]{2}),([A-Z0-9]{2}),([0-9T:-]+),([0-9T:-]+)`)
	reInfoPax  = regexp.MustCompile(`^PX=(\d+)/(\d+)/(\d+)$`)
)

// describe decodes an identifier into trip data. Fields that the identifier
// does not carry are left at their zero value; callers fall back to defaults.
func describe(id string) offerInfo {
	info := offerInfo{Raw: id}
	for _, s := range parseOfferID(id).segs {
		t := s.text
		if g := reInfoTrip.FindStringSubmatch("|" + t + "|"); g != nil && info.Legs == nil {
			info.setLegs(parseLegs(g[1]))
		}
		if g := reInfoFlt.FindStringSubmatch(t); g != nil {
			info.FlightNum, info.Carrier = g[3], g[4]
			info.DepTime, info.ArrTime = g[6], g[7]
			if info.Origin == "" {
				info.Origin, info.Dest = g[1], g[2]
			}
		}
		if g := rePrice.FindStringSubmatch(t); g != nil {
			info.Currency = g[1]
			info.Amount = atoi(g[2])
		}
		if g := reInfoPax.FindStringSubmatch(t); g != nil {
			info.ADT, info.CHD, info.INF = atoi(g[1]), atoi(g[2]), atoi(g[3])
		}
	}
	info.applyDefaults()
	return info
}

// setLegs records an itinerary and mirrors its first leg and any return into
// the single-trip fields the builders read.
func (i *offerInfo) setLegs(legs []leg) {
	i.Legs = legs
	if len(legs) == 0 {
		return
	}
	i.Origin, i.Dest, i.DepDate = legs[0].From, legs[0].To, legs[0].Date
	i.RoundTrip = pattern(legs) == "0-1,1-0"
	i.RetDate = ""
	if i.RoundTrip {
		i.RetDate = legs[1].Date
	}
}

// applyDefaults fills in anything an opaque identifier did not carry, so the
// mock still answers with a coherent trip instead of empty elements.
func (i *offerInfo) applyDefaults() {
	if i.Origin == "" {
		i.Origin, i.Dest = "AAA", "BBB"
	}
	if i.DepDate == "" {
		i.DepDate = "2026-01-01"
	}
	if i.Carrier == "" {
		i.Carrier = "XX"
	}
	if i.FlightNum == "" {
		i.FlightNum = "1000"
	}
	if i.DepTime == "" {
		i.DepTime = i.DepDate + "T08:00:00"
	}
	if i.ArrTime == "" {
		i.ArrTime = i.DepDate + "T12:00:00"
	}
	if i.Currency == "" {
		i.Currency = "USD"
	}
	if i.Amount == 0 {
		i.Amount = 100000
	}
	if i.ADT+i.CHD+i.INF == 0 {
		i.ADT = 1
	}
	if len(i.Legs) == 0 {
		i.Legs = []leg{{i.Origin, i.Dest, i.DepDate}}
		if i.RoundTrip && i.RetDate != "" {
			i.Legs = append(i.Legs, leg{i.Dest, i.Origin, i.RetDate})
		}
	}
}

func (i offerInfo) totalPax() int { return i.ADT + i.CHD + i.INF }

// paxIDs lists the passenger identifiers in the order the response declares
// them, matching the ADT_n / CHD_n / INF_n convention used in requests.
func (i offerInfo) paxIDs() []string {
	var out []string
	for n := 1; n <= i.ADT; n++ {
		out = append(out, "ADT_"+strconv.Itoa(n))
	}
	for n := 1; n <= i.CHD; n++ {
		out = append(out, "CHD_"+strconv.Itoa(n))
	}
	for n := 1; n <= i.INF; n++ {
		out = append(out, "INF_"+strconv.Itoa(n))
	}
	return out
}

func ptcOf(paxID string) string {
	p, _, _ := strings.Cut(paxID, "_")
	return p
}
