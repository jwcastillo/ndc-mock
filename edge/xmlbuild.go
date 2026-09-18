package main

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// Builders for the responses that have no reference file. They are assembled
// from the trip data the offer identifier carries, so the result stays
// coherent with what was asked without needing a captured payload.
//
// Everything appends into a caller-supplied buffer and the handler does a
// single Write, for the same reason the AirShopping path does.

const iataNS = "http://www.iata.org/IATA/2015/00/2019.2/"

func envelope(b []byte, root string, inner func([]byte) []byte) []byte {
	b = append(b, `<?xml version="1.0" encoding="UTF-8"?>`...)
	b = fmt.Appendf(b, `<%s xmlns="%s%s">`, root, iataNS, root)
	b = inner(b)
	return fmt.Appendf(b, `</%s>`, root)
}

func el(b []byte, name, val string) []byte {
	return fmt.Appendf(b, "<%s>%s</%s>", name, escape(val), name)
}

func eli(b []byte, name string, val int) []byte {
	return el(b, name, strconv.Itoa(val))
}

func escape(s string) string {
	if !strings.ContainsAny(s, "<>&\"") {
		return s
	}
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func bumpFlight(f string) string {
	n, err := strconv.Atoi(f)
	if err != nil {
		return f
	}
	return strconv.Itoa(n + 1)
}

// priceBlock renders a Price with a base/tax split.
func priceBlock(b []byte, cur string, total int) []byte {
	taxes := total * 18 / 100
	b = append(b, "<Price>"...)
	b = fmt.Appendf(b, `<TotalAmount CurCode="%s">%d</TotalAmount>`, cur, total)
	b = fmt.Appendf(b, `<BaseAmount CurCode="%s">%d</BaseAmount>`, cur, total-taxes)
	b = append(b, "<TaxSummary>"...)
	b = fmt.Appendf(b, `<TotalTaxAmount CurCode="%s">%d</TotalTaxAmount>`, cur, taxes)
	b = append(b, "</TaxSummary></Price>"...)
	return b
}

// --- 19.2 documents as trees -----------------------------------------------
//
// OfferPrice and the order messages are built as trees in 19.2 shape, valid
// against the 19.2 schema once sorted, and served in later generations
// through the same mapping the AirShopping reference is translated with.

func node(name string, kids ...*xnode) *xnode { return &xnode{name: name, kids: kids} }

func amount(name, cur string, v int) *xnode {
	n := leaf(name, strconv.Itoa(v))
	n.attrs = []xml.Attr{{Name: xml.Name{Local: "CurCode"}, Value: cur}}
	return n
}

func ndcRoot(message string, kids ...*xnode) *xnode {
	n := node(message, kids...)
	n.attrs = []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: iataNS + message}}
	return n
}

// price is PriceType: total, base and the tax on top.
func price(name, cur string, total int) *xnode {
	taxes := total * 18 / 100
	return node(name, amount("BaseAmount", cur, total-taxes),
		node("TaxSummary", amount("TotalTaxAmount", cur, taxes)),
		amount("TotalAmount", cur, total))
}

// price2 is Price2Type, which carries no tax breakdown.
func price2(name, cur string, total int) *xnode {
	return node(name, amount("BaseAmount", cur, total-total*18/100), amount("TotalAmount", cur, total))
}

type flightLeg struct {
	from, to, dep, arr, flight string
}

// legs is one flight per leg of the itinerary: the first as the identifier
// describes it, the others on their own dates with the next flight numbers.
func (i offerInfo) legs() []flightLeg {
	out := []flightLeg{{i.Origin, i.Dest, i.DepTime, i.ArrTime, i.FlightNum}}
	flight := i.FlightNum
	for _, l := range i.Legs[min(1, len(i.Legs)):] {
		flight = bumpFlight(flight)
		out = append(out, flightLeg{l.From, l.To, l.Date + "T10:00:00", l.Date + "T14:00:00", flight})
	}
	return out
}

func journeyID(n int) string { return "PJ_" + strconv.Itoa(n+1) }

// dataLists carries the passengers and one journey per direction, each of a
// single segment, with the origin-destination pairs that group them.
func dataLists(i offerInfo, pax []passenger) *xnode {
	pl := node("PaxList")
	for _, p := range pax {
		pl.kids = append(pl.kids, node("Pax",
			node("Individual", leaf("Birthdate", p.Birthdate), leaf("GivenName", p.Given), leaf("Surname", p.Surname)),
			leaf("PaxID", p.ID), leaf("PTC", p.PTC)))
	}
	segs, journeys, ods := node("PaxSegmentList"), node("PaxJourneyList"), node("OriginDestList")
	for n, l := range i.legs() {
		segID := "SEG_" + l.from + "_" + l.to + "_" + l.flight
		segs.kids = append(segs.kids, node("PaxSegment",
			node("Arrival", leaf("AircraftScheduledDateTime", l.arr), leaf("IATA_LocationCode", l.to)),
			node("Dep", leaf("AircraftScheduledDateTime", l.dep), leaf("IATA_LocationCode", l.from)),
			node("MarketingCarrierInfo", leaf("CarrierDesigCode", i.Carrier), leaf("MarketingCarrierFlightNumberText", l.flight)),
			node("OperatingCarrierInfo", leaf("CarrierDesigCode", i.Carrier)),
			leaf("PaxSegmentID", segID)))
		journeys.kids = append(journeys.kids, node("PaxJourney", leaf("PaxJourneyID", journeyID(n)), leaf("PaxSegmentRefID", segID)))
		ods.kids = append(ods.kids, node("OriginDest", leaf("DestCode", l.to), leaf("OriginCode", l.from),
			leaf("OriginDestID", "OD_"+strconv.Itoa(n+1)), leaf("PaxJourneyRefID", journeyID(n))))
	}
	return node("DataLists", ods, journeys, pl, segs)
}

// flightService is the flight itself as a service: every passenger, associated
// with every journey of the trip.
func flightService(i offerInfo, id string) *xnode {
	s := node("Service")
	for _, p := range i.paxIDs() {
		s.kids = append(s.kids, leaf("PaxRefID", p))
	}
	assoc := node("ServiceAssociations")
	for n := range i.legs() {
		assoc.kids = append(assoc.kids, leaf("PaxJourneyRefID", journeyID(n)))
	}
	return node("Service", append(s.kids, assoc, leaf("ServiceID", id))...)
}

// offer renders a priced offer: OfferItem with a Price in OfferPrice.
func offer(name, offerID, itemID string, i offerInfo) *xnode {
	return node(name,
		leaf("OfferID", offerID),
		node("OfferItem", leaf("OfferItemID", itemID), price2("Price", i.Currency, i.Amount), flightService(i, "SVC_1")),
		leaf("OwnerCode", i.Carrier),
		price("TotalPrice", i.Currency, i.Amount))
}

// reshopOffer is the same offer as OrderReshop shapes it: the item is added
// to the order, so it is an AddOfferItem priced through ReshopPrice/AddPrice.
func reshopOffer(offerID, itemID string, i offerInfo) *xnode {
	return node("Offer",
		node("AddOfferItem", leaf("OfferItemID", itemID),
			node("ReshopPrice", node("AddPrice", price("Price", i.Currency, i.Amount))),
			flightService(i, "SVC_1")),
		leaf("OfferID", offerID),
		leaf("OwnerCode", i.Carrier),
		price("TotalPrice", i.Currency, i.Amount))
}

// orderServices are what an order item holds for a flight: in an order each
// service is one passenger on one segment.
func orderServices(i offerInfo, prefix string) []*xnode {
	var out []*xnode
	for _, p := range i.paxIDs() {
		for _, l := range i.legs() {
			out = append(out, node("Service",
				leaf("PaxRefID", p),
				node("ServiceAssociations", leaf("PaxSegmentRefID", "SEG_"+l.from+"_"+l.to+"_"+l.flight)),
				leaf("ServiceID", prefix+p+"_"+l.from+l.to)))
		}
	}
	return out
}

// aLaCarteItem is an ancillary offered on its own: eligible for the given
// passengers on the given segments, pointing at its service definition.
func aLaCarteItem(itemID, defID, svcID string, pax, segs []string, unit *xnode) *xnode {
	elig := node("Eligibility", node("FlightAssociations"))
	for _, sg := range segs {
		elig.kids[0].kids = append(elig.kids[0].kids, leaf("PaxSegmentRefID", sg))
	}
	for _, p := range pax {
		elig.kids = append(elig.kids, leaf("PaxRefID", p))
	}
	return node("ALaCarteOfferItem", elig, leaf("OfferItemID", itemID),
		node("Service", leaf("ServiceDefinitionRefID", defID), leaf("ServiceID", svcID)), unit)
}

func serviceDefinition(id, code, name, owner string) *xnode {
	return node("ServiceDefinition", node("Desc", leaf("DescText", name)), leaf("Name", name),
		leaf("OwnerCode", owner), leaf("ServiceCode", code), leaf("ServiceDefinitionID", id))
}

func (i offerInfo) segmentIDs() []string {
	var out []string
	for _, l := range i.legs() {
		out = append(out, "SEG_"+l.from+"_"+l.to+"_"+l.flight)
	}
	return out
}

// withDefinitions appends a ServiceDefinitionList to the data lists.
func withDefinitions(dl *xnode, defs []*xnode) *xnode {
	if len(defs) > 0 {
		dl.kids = append(dl.kids, node("ServiceDefinitionList", defs...))
	}
	return dl
}

// orderStatus maps the store's states onto the 19.2 order status code list.
var orderStatus = map[string]string{"Opened": "OPENED", "Changed": "OPENED", "Cancelled": "CLOSED"}

func orderNode(o order) *xnode {
	item := node("OrderItem", append([]*xnode{leaf("OrderItemID", o.ID+"-ITEM-1"), leaf("OwnerCode", o.Info.Carrier),
		price("Price", o.Info.Currency, o.Info.Amount)}, orderServices(o.Info, "SVC_")...)...)
	ord := node("Order", leaf("OrderID", o.ID), item)
	for n, s := range o.Services {
		ord.kids = append(ord.kids, node("OrderItem", append([]*xnode{leaf("OrderItemID", o.ID+"-SVC-"+strconv.Itoa(n+1)), leaf("OwnerCode", o.Info.Carrier),
			price("Price", s.Currency, s.Amount)}, orderServices(o.Info, s.ID+"_")...)...))
	}
	return node("Order", append(ord.kids,
		leaf("OwnerCode", o.Info.Carrier),
		leaf("StatusCode", orderStatus[o.Status]),
		price("TotalPrice", o.Info.Currency, o.total()))...)
}

func (o order) total() int {
	t := o.Info.Amount
	for _, s := range o.Services {
		t += s.Amount
	}
	for _, s := range o.Seats {
		t += s.Amount
	}
	return t
}
