package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The sampled distribution has to land on the quantiles it was configured with.
func TestLatencyQuantiles(t *testing.T) {
	l, err := parseDelay("800ms~2400ms")
	if err != nil {
		t.Fatal(err)
	}
	s := make([]time.Duration, 50000)
	for i := range s {
		s[i] = l.sample()
	}
	slices.Sort(s)
	near := func(got, want time.Duration) bool { return got > want*95/100 && got < want*105/100 }
	if p50 := s[len(s)/2]; !near(p50, 800*time.Millisecond) {
		t.Errorf("p50 = %v, want ~800ms", p50)
	}
	if p95 := s[len(s)*95/100]; !near(p95, 2400*time.Millisecond) {
		t.Errorf("p95 = %v, want ~2.4s", p95)
	}
	for _, bad := range []string{"2s~1s", "0s~1s", "x~1s"} {
		if _, err := parseDelay(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if d, _ := parseDelay("300ms"); d.sample() != 300*time.Millisecond {
		t.Error("a fixed delay must stay fixed")
	}
}

func TestItineraryShapes(t *testing.T) {
	leg := func(from, to string) string {
		return `<OriginDestCriteria><OriginDepCriteria><Date>2026-10-01</Date><IATA_LocationCode>` + from +
			`</IATA_LocationCode></OriginDepCriteria><DestArrivalCriteria><IATA_LocationCode>` + to +
			`</IATA_LocationCode></DestArrivalCriteria></OriginDestCriteria>`
	}
	rq := func(legs ...string) []byte {
		return []byte(`<IATA_AirShoppingRQ><Request><FlightCriteria>` + strings.Join(legs, "") +
			`</FlightCriteria><Paxs><Pax><PTC>ADT</PTC></Pax></Paxs></Request></IATA_AirShoppingRQ>`)
	}
	for name, c := range map[string]struct {
		body  []byte
		shape string
		multi bool
	}{
		"one way":    {rq(leg("GRU", "SCL")), "0-1", false},
		"return":     {rq(leg("GRU", "SCL"), leg("SCL", "GRU")), "0-1,1-0", false},
		"open jaw":   {rq(leg("GRU", "SCL"), leg("LIM", "GRU")), "0-1,2-0", true},
		"circuit":    {rq(leg("SCL", "LIM"), leg("LIM", "BOG"), leg("BOG", "SCL")), "0-1,1-2,2-0", true},
		"open chain": {rq(leg("SCL", "LIM"), leg("LIM", "BOG"), leg("BOG", "MIA")), "0-1,1-2,2-3", true},
	} {
		q, err := parseQuery(c.body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := pattern(q.Legs); got != c.shape || q.multiCity() != c.multi {
			t.Errorf("%s: shape %s multi %v, want %s %v", name, got, q.multiCity(), c.shape, c.multi)
		}
	}
}

// 21.3 wrapped the shopping criteria in FlightRequest and renamed Paxs to
// PaxList; a real 21.3+ client sends that shape, in the common-types namespace.
func TestParseQuery213(t *testing.T) {
	body := []byte(`<m:IATA_AirShoppingRQ xmlns="urn:types" xmlns:m="urn:msg"><m:Request><FlightRequest>` +
		`<FlightRequestOriginDestinationsCriteria><OriginDestCriteria><DestArrivalCriteria><IATA_LocationCode>NAT</IATA_LocationCode></DestArrivalCriteria>` +
		`<OriginDepCriteria><Date>2026-09-30</Date><IATA_LocationCode>GRU</IATA_LocationCode></OriginDepCriteria></OriginDestCriteria>` +
		`</FlightRequestOriginDestinationsCriteria></FlightRequest>` +
		`<PaxList><Pax><PaxID>ADT_1</PaxID><PTC>ADT</PTC></Pax><Pax><PaxID>INF_1</PaxID><PTC>INF</PTC></Pax></PaxList></m:Request></m:IATA_AirShoppingRQ>`)
	q, err := parseQuery(body)
	if err != nil {
		t.Fatal(err)
	}
	if q.Origin != "GRU" || q.Dest != "NAT" || q.DepDate != "2026-09-30" || q.ADT != 1 || q.INF != 1 {
		t.Errorf("got %+v", q)
	}
}

// IATA requests point at offers and orders by reference; provider dialects
// often use the direct names. Both resolve, the direct name winning.
func TestScanIDsRefAliases(t *testing.T) {
	ids := scanIDs([]byte(`<RQ><SelectedOffer><OfferRefID>O1</OfferRefID><SelectedOfferItem><OfferItemRefID>I1</OfferItemRefID></SelectedOfferItem></SelectedOffer><OrderRefID>R1</OrderRefID></RQ>`))
	if ids.OfferID != "O1" || ids.OfferItemID != "I1" || ids.OrderID != "R1" {
		t.Errorf("got %+v", ids)
	}
	if ids = scanIDs([]byte(`<RQ><OrderRefID>R1</OrderRefID><OrderID>D1</OrderID></RQ>`)); ids.OrderID != "D1" {
		t.Errorf("direct OrderID must win, got %q", ids.OrderID)
	}
}
