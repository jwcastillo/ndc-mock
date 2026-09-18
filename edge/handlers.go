package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Handlers for everything after AirShopping. None of these needs a reference
// response: the offer identifier is self-describing, so the trip can be rebuilt
// from it, and the order store carries the rest.

var orders *orderStore

// reqIDs pulls whatever identifiers a request carries. The operations differ in
// element name but not in shape, so one scan covers them all.
type reqIDs struct {
	OfferID     string `xml:"-"`
	OfferItemID string `xml:"-"`
	OrderID     string `xml:"-"`
	ShoppingID  string `xml:"-"`
}

// scanIDs picks the identifiers out of any request by element name. The IATA
// requests point at an offer or order by reference (OfferRefID, OfferItemRefID,
// OrderRefID); providers' own dialects often carry OfferID and OrderID. Both are
// read, the direct name winning when a request carries both.
func scanIDs(body []byte) reqIDs {
	var ids reqIDs
	found := map[string]string{}
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	var cur string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			cur = t.Name.Local
		case xml.CharData:
			if v := strings.TrimSpace(string(t)); v != "" && found[cur] == "" {
				found[cur] = v
			}
		}
	}
	ids.OfferID = firstNonEmpty(found["OfferID"], found["OfferRefID"])
	ids.OfferItemID = firstNonEmpty(found["OfferItemID"], found["OfferItemRefID"])
	ids.OrderID = firstNonEmpty(found["OrderID"], found["OrderRefID"])
	ids.ShoppingID = firstNonEmpty(found["ShoppingResponseID"], found["ShoppingResponseRefID"])
	return ids
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		failure(w, r, http.StatusBadRequest, "could not read request body")
		return nil, false
	}
	return b, true
}

// infoFrom rebuilds the trip from whichever identifier the request carried,
// applying the per-request carrier profile.
func infoFrom(r *http.Request, ids reqIDs) offerInfo {
	id := ids.OfferItemID
	if id == "" {
		id = ids.OfferID
	}
	info := describe(id)
	if air := airs.get(r.Header.Get("X-Mock-Airline")); air != nil {
		info.Carrier = air.Code
		info.Currency, info.Amount = air.convert(info.Currency, info.Amount)
	}
	return info
}

// --- OfferPrice -------------------------------------------------------------

func offerPrice(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	ids := scanIDs(body)
	if ids.OfferID == "" && ids.OfferItemID == "" {
		failure(w, r, http.StatusBadRequest, "request carries no OfferID or OfferItemID")
		return
	}
	info := infoFrom(r, ids)
	applyDelay(r)

	if formatOf(r) == formatJSON {
		writeJSON(w, http.StatusOK, map[string]any{
			"pricedOffer": offerToJSON(orDefault(ids.OfferID, ids.OfferItemID), info),
			"repriced":    false,
		})
		return
	}
	respondNDC(w, r, http.StatusOK, ndcRoot("IATA_OfferPriceRS", node("Response",
		dataLists(info, buildPax(info)),
		offer("PricedOffer", orDefault(ids.OfferID, ids.OfferItemID), orDefault(ids.OfferItemID, "ITEM_1"), info))))
}

// --- OrderCreate ------------------------------------------------------------

func orderCreate(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	ids := scanIDs(body)
	if ids.OfferID == "" && ids.OfferItemID == "" {
		failure(w, r, http.StatusBadRequest, "request carries no offer to order")
		return
	}
	info := infoFrom(r, ids)
	o := &order{
		ID:        newOrderID(info.Carrier),
		Status:    "Opened",
		Info:      info,
		Pax:       buildPax(info),
		CreatedAt: time.Now(),
		History:   []string{"Created"},
	}
	orders.put(o)
	applyDelay(r)
	respondOrder(w, r, o.clone(), "IATA_OrderViewRS", http.StatusCreated)
}

// --- OrderRetrieve / OrderCancel / OrderChange / OrderReshop ----------------

func orderRetrieve(w http.ResponseWriter, r *http.Request) {
	o, ok := lookup(w, r, nil)
	if !ok {
		return
	}
	applyDelay(r)
	respondOrder(w, r, o, "IATA_OrderViewRS", http.StatusOK)
}

func orderCancel(w http.ResponseWriter, r *http.Request) {
	o, ok := lookup(w, r, func(o *order) {
		o.Status = "Cancelled"
		o.History = append(o.History, "Cancelled")
	})
	if !ok {
		return
	}
	applyDelay(r)
	// From 20.1 IATA has no OrderCancel message: an order is cancelled through
	// OrderChange and answered with OrderView. Only 19.2 gets OrderCancelRS.
	if v := vers.Versions[versionOf(r.URL.Path)]; formatOf(r) == formatJSON || v != nil && v.IATA != "19.2" {
		respondOrder(w, r, o, "IATA_OrderViewRS", http.StatusOK)
		return
	}
	respondNDC(w, r, http.StatusOK, ndcRoot("IATA_OrderCancelRS", node("Response", leaf("OrderRefID", o.ID))))
}

func orderChange(w http.ResponseWriter, r *http.Request) {
	o, ok := lookup(w, r, func(o *order) {
		o.Status = "Changed"
		o.History = append(o.History, "Changed")
	})
	if !ok {
		return
	}
	applyDelay(r)
	respondOrder(w, r, o, "IATA_OrderViewRS", http.StatusOK)
}

// orderReshop offers alternatives to an existing order: the same trip at a
// spread of fares, which is what a reshop returns.
func orderReshop(w http.ResponseWriter, r *http.Request) {
	o, ok := lookup(w, r, nil)
	if !ok {
		return
	}
	n := offerCountFor(r, 5)
	applyDelay(r)

	if formatOf(r) == formatJSON {
		out := make([]jsonOffer, 0, n)
		for k := 0; k < n; k++ {
			alt := o.Info
			alt.Amount = o.Info.Amount + k*15000
			out = append(out, offerToJSON(fmt.Sprintf("%s-ALT-%d", o.ID, k+1), alt))
		}
		writeJSON(w, http.StatusOK, map[string]any{"orderId": o.ID, "alternatives": out})
		return
	}
	results := node("ReshopOffers")
	for k := 0; k < n; k++ {
		alt := o.Info
		alt.Amount = o.Info.Amount + k*15000
		results.kids = append(results.kids, reshopOffer(
			fmt.Sprintf("%s-ALT-%d", o.ID, k+1), fmt.Sprintf("ITEM-%d", k+1), alt))
	}
	resp := node("Response", dataLists(o.Info, o.Pax), node("ReshopResults", results))
	// From 21.3 the response names the order it reshops; 19.2 has no place for it.
	if v := vers.Versions[versionOf(r.URL.Path)]; v != nil && v.IATA != "19.2" {
		resp.kids = append(resp.kids, node("Order", leaf("OrderID", o.ID), leaf("OwnerCode", o.Info.Carrier)))
	}
	respondNDC(w, r, http.StatusOK, ndcRoot("IATA_OrderReshopRS", resp))
}

// --- ServiceList / SeatAvailability ----------------------------------------

// serviceList returns ancillaries. Count is configurable because payload size,
// not request rate, is what a load test is usually probing.
func serviceList(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	info := infoFrom(r, scanIDs(body))
	n := offerCountFor(r, 12)
	svcs := synthServices(info, n)
	applyDelay(r)

	if formatOf(r) == formatJSON {
		writeJSON(w, http.StatusOK, map[string]any{"services": svcs, "count": len(svcs)})
		return
	}
	var items, defs []*xnode
	seen := map[string]bool{}
	for _, sv := range svcs {
		defID := "SD_" + sv.Code
		if !seen[defID] {
			seen[defID] = true
			defs = append(defs, serviceDefinition(defID, sv.Code, sv.Name, info.Carrier))
		}
		items = append(items, aLaCarteItem(sv.ID, defID, sv.ID, []string{sv.PaxID}, info.segmentIDs(),
			price2("UnitPrice", sv.Currency, sv.Amount)))
	}
	respondNDC(w, r, http.StatusOK, ndcRoot("IATA_ServiceListRS", node("Response",
		node("ALaCarteOffer", append(items, leaf("OfferID", "ALC_"+info.segmentIDs()[0]), leaf("OwnerCode", info.Carrier))...),
		withDefinitions(dataLists(info, buildPax(info)), defs))))
}

func seatAvailability(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	info := infoFrom(r, scanIDs(body))
	rows := offerCountFor(r, 30)
	applyDelay(r)

	cols := []string{"A", "B", "C", "D", "E", "F"}
	if formatOf(r) == formatJSON {
		type js struct {
			Row       int    `json:"row"`
			Column    string `json:"column"`
			Available bool   `json:"available"`
			Currency  string `json:"currency"`
			Amount    int    `json:"amount"`
		}
		out := make([]js, 0, rows*len(cols))
		for row := 1; row <= rows; row++ {
			for ci, c := range cols {
				out = append(out, js{row, c, (row+ci)%4 != 0, info.Currency, seatPrice(info, row)})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"cabin": "Y", "seats": out, "count": len(out)})
		return
	}
	// Seats are priced through a la carte items, one per price tier; a seat
	// points at the item for its row. Status codes are IATA's PADIS ones.
	seg := info.segmentIDs()[0]
	tier := func(row int) string { return fmt.Sprintf("SEAT_%d", seatPrice(info, row)) }
	var items []*xnode
	priced := map[string]bool{}
	cabin := node("CabinCompartment", node("CabinType", leaf("CabinTypeCode", "Y")))
	for _, c := range cols {
		cabin.kids = append(cabin.kids, leaf("ColumnID", c))
	}
	cabin.kids = append(cabin.kids, leaf("FirstRowNumber", "1"), leaf("LastRowNumber", strconv.Itoa(rows)))
	for row := 1; row <= rows; row++ {
		if id := tier(row); !priced[id] {
			priced[id] = true
			items = append(items, aLaCarteItem(id, "SD_SEAT", id, info.paxIDs(), []string{seg},
				price2("UnitPrice", info.Currency, seatPrice(info, row))))
		}
		sr := node("SeatRow", leaf("RowNumber", strconv.Itoa(row)))
		for ci, c := range cols {
			sr.kids = append(sr.kids, node("Seat", leaf("ColumnID", c),
				leaf("OccupationStatusCode", occupied((row+ci)%4 == 0)), leaf("OfferItemRefID", tier(row))))
		}
		cabin.kids = append(cabin.kids, sr)
	}
	respondNDC(w, r, http.StatusOK, ndcRoot("IATA_SeatAvailabilityRS", node("Response",
		node("ALaCarteOffer", append(items, leaf("OfferID", "SEATS_"+seg), leaf("OwnerCode", info.Carrier))...),
		withDefinitions(dataLists(info, buildPax(info)), []*xnode{serviceDefinition("SD_SEAT", "SEAT", "Seat selection", info.Carrier)}),
		node("SeatMap", cabin, leaf("PaxSegmentRefID", seg)))))
}

// occupied answers in IATA's seat status codes: O occupied, F free.
func occupied(v bool) string {
	if v {
		return "O"
	}
	return "F"
}

// seatPrice makes front rows dearer, the way carriers price them.
func seatPrice(i offerInfo, row int) int {
	base := i.Amount / 40
	if row <= 4 {
		return base * 3
	}
	if row <= 10 {
		return base * 2
	}
	return base
}

func synthServices(i offerInfo, n int) []service {
	catalog := []struct{ code, name string }{
		{"BAG", "Checked bag"}, {"SEAT", "Seat selection"}, {"MEAL", "Meal"},
		{"WIFI", "Wi-Fi"}, {"LNGE", "Lounge access"}, {"PETC", "Pet in cabin"},
		{"SPEQ", "Sports equipment"}, {"PRIO", "Priority boarding"},
	}
	ids := i.paxIDs()
	out := make([]service, 0, n)
	for k := 0; k < n; k++ {
		c := catalog[k%len(catalog)]
		out = append(out, service{
			ID:       fmt.Sprintf("SVC_%s_%d", c.code, k+1),
			Code:     c.code,
			Name:     c.name,
			PaxID:    ids[k%len(ids)],
			Currency: i.Currency,
			Amount:   i.Amount / 20 * (1 + k%3),
		})
	}
	return out
}

// --- shared -----------------------------------------------------------------

// lookup finds the order a request names and, when fn is given, applies it as
// a state change. An order held by another replica is forwarded there and
// lookup reports false, the response having already been written.
func lookup(w http.ResponseWriter, r *http.Request, fn func(*order)) (order, bool) {
	body, ok := readBody(w, r)
	if !ok {
		return order{}, false
	}
	id := scanIDs(body).OrderID
	if id == "" {
		id = r.URL.Query().Get("orderId")
	}
	if id == "" {
		failure(w, r, http.StatusBadRequest, "request carries no OrderID")
		return order{}, false
	}
	var o order
	var found bool
	if fn != nil {
		o, found = orders.mutate(id, fn)
	} else {
		o, found = orders.get(id)
	}
	if found {
		return o, true
	}
	if owner := ownerOf(id); owner != "" && owner != *advertise &&
		r.Header.Get("X-Mock-Forwarded") == "" && isPeer(owner) {
		forward(w, r, owner, body)
		return order{}, false
	}
	failure(w, r, http.StatusNotFound, "unknown OrderID "+id+
		" (the store is in-memory and entries expire)")
	return order{}, false
}

func respondOrder(w http.ResponseWriter, r *http.Request, o order, root string, status int) {
	if formatOf(r) == formatJSON {
		writeJSON(w, status, map[string]any{
			"orderId":  o.ID,
			"status":   o.Status,
			"currency": o.Info.Currency,
			"total":    o.total(),
			"offer":    offerToJSON(o.ID, o.Info),
			"history":  o.History,
		})
		return
	}
	respondNDC(w, r, status, ndcRoot(root, node("Response", dataLists(o.Info, o.Pax), orderNode(o))))
}

// respondNDC writes a document built in 19.2 shape in the generation the URL
// asks for. 19.2 only needs its children sorted: its sequences are alphabetical
// too, apart from a few service-definition types these builders do not emit.
// Later generations go through the mapping the AirShopping reference uses.
func respondNDC(w http.ResponseWriter, r *http.Request, status int, root *xnode) {
	v := vers.Versions[versionOf(r.URL.Path)]
	if v == nil || v.IATA == "19.2" {
		root.sortBelowRoot()
		writeXML(w, status, root.serialize())
		return
	}
	m := maps.find("19.2", v.IATA)
	if !m.usable() {
		failure(w, r, http.StatusNotImplemented, "no translation mapping from 19.2 to "+v.IATA)
		return
	}
	m.translateTree(root)
	w.Header().Set("X-Mock-Translated", m.From+"->"+m.To)
	writeXML(w, status, root.serialize())
}

// offerCountFor lets a load test size the payload per request.
func offerCountFor(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.Header.Get("X-Mock-Offers")); err == nil && n > 0 {
		return clampOffers(n, *maxOffers)
	}
	return def
}

// applyDelay sleeps for the X-Mock-Delay header if given, otherwise for the
// operation's configured latency.
func applyDelay(r *http.Request) {
	d := cfg.opDelay(opOf(r.URL.Path))
	if h, err := parseDelay(r.Header.Get("X-Mock-Delay")); err == nil && h.p50 > 0 {
		d = h.sample()
	}
	if d > 0 {
		time.Sleep(clampDelay(d, *maxDelay))
	}
}

// opOf pulls the operation out of /ndc/<version>/<operation...>.
func opOf(path string) string {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// failure answers in whichever format the caller asked for.
func failure(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if formatOf(r) == formatJSON {
		writeJSON(w, code, jsonError{Code: code, Message: "ndc-mock: " + msg, Owner: "NDC_MOCK"})
		return
	}
	fault(w, code, msg)
}

// --- Installments -----------------------------------------------------------

// installments returns a payment plan for the offer, the way a carrier selling
// in instalment markets does.
func installments(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	info := infoFrom(r, scanIDs(body))
	plans := []int{1, 3, 6, 9, 12}
	applyDelay(r)

	type plan struct {
		Count    int    `json:"count"`
		Currency string `json:"currency"`
		Each     int    `json:"each"`
		Total    int    `json:"total"`
		Interest bool   `json:"interest"`
	}
	build := func(n int) plan {
		total := info.Amount
		if n > 6 {
			total = total * 108 / 100 // instalments beyond six carry interest
		}
		return plan{n, info.Currency, total / n, total, n > 6}
	}

	if formatOf(r) == formatJSON {
		out := make([]plan, 0, len(plans))
		for _, n := range plans {
			out = append(out, build(n))
		}
		writeJSON(w, http.StatusOK, map[string]any{"options": out})
		return
	}
	b := envelope(nil, "IATA_OrderRulesRS", func(b []byte) []byte {
		b = append(b, "<Response><PaymentPlanList>"...)
		for _, n := range plans {
			p := build(n)
			b = append(b, "<PaymentPlan>"...)
			b = eli(b, "InstallmentQty", p.Count)
			b = priceBlock(b, p.Currency, p.Total)
			b = append(b, "<InstallmentAmount>"...)
			b = fmt.Appendf(b, `<TotalAmount CurCode="%s">%d</TotalAmount>`, p.Currency, p.Each)
			b = append(b, "</InstallmentAmount>"...)
			b = el(b, "InterestInd", strconv.FormatBool(p.Interest))
			b = append(b, "</PaymentPlan>"...)
		}
		return append(b, "</PaymentPlanList></Response>"...)
	})
	writeXML(w, http.StatusOK, b)
}
