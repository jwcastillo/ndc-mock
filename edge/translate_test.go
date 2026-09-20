package main

import (
	"strings"
	"testing"
)

func translateString(t *testing.T, m *mapping, in string) string {
	t.Helper()
	out, _, err := m.translate([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(out)), "")
}

func TestScopedRenameWrapSortValues(t *testing.T) {
	m := &mapping{
		Namespaces:   map[string]string{"urn:old": "urn:types"},
		Declare:      map[string]string{"m": "urn:msg"},
		Rename:       map[string]string{"R": "m:R", "R/Response": "m:Response", "Offer/BaggageAllowance": "BaggageAssociations", "Price": "FareAmount"},
		Drop:         []string{"Penalty/AppCode"},
		Wrap:         map[string]string{"Assoc/PaxSegmentRefID": "PaxSegmentReferences"},
		Values:       map[string]map[string]string{"Code": {"SELL_AMOUNT": "Filed"}},
		SortChildren: true,
	}
	in := `<R xmlns="urn:old"><Response><Offer><Zeta>1</Zeta><BaggageAllowance><Assoc><PaxSegmentRefID>a</PaxSegmentRefID><PaxSegmentRefID>b</PaxSegmentRefID></Assoc></BaggageAllowance><Code>SELL_AMOUNT</Code></Offer>` +
		`<DataLists><BaggageAllowance><X>1</X></BaggageAllowance><Penalty><AppCode>1</AppCode></Penalty><PriceClass>p</PriceClass><Price>9</Price></DataLists></Response></R>`
	want := `<?xmlversion="1.0"encoding="UTF-8"standalone="no"?><m:Rxmlns="urn:types"xmlns:m="urn:msg"><m:Response>` +
		`<DataLists><BaggageAllowance><X>1</X></BaggageAllowance><FareAmount>9</FareAmount><Penalty></Penalty><PriceClass>p</PriceClass></DataLists>` +
		`<Offer><BaggageAssociations><Assoc><PaxSegmentReferences><PaxSegmentRefID>a</PaxSegmentRefID><PaxSegmentRefID>b</PaxSegmentRefID></PaxSegmentReferences></Assoc></BaggageAssociations><Code>Filed</Code><Zeta>1</Zeta></Offer>` +
		`</m:Response></m:R>`
	if got := translateString(t, m, in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestDatedSegments(t *testing.T) {
	in := `<R><DataLists><PaxSegmentList><PaxSegment>` +
		`<Arrival><IATA_LocationCode>NAT</IATA_LocationCode></Arrival><CabinType><CabinTypeCode>Y</CabinTypeCode></CabinType>` +
		`<DatedOperatingLeg><Arrival><IATA_LocationCode>NAT</IATA_LocationCode></Arrival><Dep></Dep></DatedOperatingLeg>` +
		`<Dep><IATA_LocationCode>GRU</IATA_LocationCode></Dep><Duration>PT3H</Duration>` +
		`<MarketingCarrierInfo><CarrierDesigCode>XX</CarrierDesigCode><MarketingCarrierFlightNumberText>3438</MarketingCarrierFlightNumberText></MarketingCarrierInfo>` +
		`<OperatingCarrierInfo><CarrierDesigCode>YY</CarrierDesigCode></OperatingCarrierInfo><PaxSegmentID>S1</PaxSegmentID>` +
		`</PaxSegment></PaxSegmentList></DataLists></R>`
	got := translateString(t, &mapping{Structural: []string{"dated-segments"}, SortChildren: true}, in)
	for _, want := range []string{
		`<PaxSegment><CabinTypeAssociationChoice><SegmentCabinType><CabinTypeCode>Y</CabinTypeCode></SegmentCabinType></CabinTypeAssociationChoice><DatedMarketingSegmentRefId>DMS_S1</DatedMarketingSegmentRefId><PaxSegmentID>S1</PaxSegmentID></PaxSegment>`,
		`<DatedMarketingSegment><Arrival><IATA_LocationCode>NAT</IATA_LocationCode></Arrival><CarrierDesigCode>XX</CarrierDesigCode><DatedMarketingSegmentId>DMS_S1</DatedMarketingSegmentId><DatedOperatingSegmentRefId>DOS_S1</DatedOperatingSegmentRefId><Dep><IATA_LocationCode>GRU</IATA_LocationCode></Dep><MarketingCarrierFlightNumberText>3438</MarketingCarrierFlightNumberText></DatedMarketingSegment>`,
		`<DatedOperatingSegment><CarrierDesigCode>YY</CarrierDesigCode><DatedOperatingLegRefID>LEG_S1_1</DatedOperatingLegRefID><DatedOperatingSegmentId>DOS_S1</DatedOperatingSegmentId><Duration>PT3H</Duration></DatedOperatingSegment>`,
		// The leg's empty Dep is filled from the segment: 21.3 requires it.
		`<DatedOperatingLeg><Arrival><IATA_LocationCode>NAT</IATA_LocationCode></Arrival><DatedOperatingLegID>LEG_S1_1</DatedOperatingLegID><Dep><IATA_LocationCode>GRU</IATA_LocationCode></Dep></DatedOperatingLeg>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing\n%s\nin\n%s", want, got)
		}
	}
}

// A mapping with nothing in it must not count as a route to a generation:
// the request answers 501 instead of an untouched document claiming a translation.
func TestEmptyMappingUnusable(t *testing.T) {
	if (&mapping{From: "19.2", To: "99.9"}).usable() {
		t.Error("empty mapping reported usable")
	}
}

func TestHoist(t *testing.T) {
	m := &mapping{Hoist: map[string]string{"FareRule/PenaltyRefID": "FareDetail"}}
	in := `<FareDetail><PenaltyRefID>A</PenaltyRefID><FareComponent><FareRule><PenaltyRefID>A</PenaltyRefID><PenaltyRefID>B</PenaltyRefID><RuleCode>X</RuleCode></FareRule></FareComponent></FareDetail>`
	want := `<?xmlversion="1.0"encoding="UTF-8"standalone="no"?><FareDetail><PenaltyRefID>A</PenaltyRefID><FareComponent><FareRule><RuleCode>X</RuleCode></FareRule></FareComponent><PenaltyRefID>B</PenaltyRefID></FareDetail>`
	if got := translateString(t, m, in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// The same element takes different names depending on where it sits: 21.3
// calls an offer's service association OfferServiceAssociation and an order's
// OrderServiceAssociation.
func TestPathSuffixRename(t *testing.T) {
	m := &mapping{Rename: map[string]string{
		"OfferItem/Service/ServiceAssociations": "OfferServiceAssociation",
		"OrderItem/Service/ServiceAssociations": "OrderServiceAssociation",
		"ServiceAssociations":                   "Other",
	}}
	in := `<R><OfferItem><Service><ServiceAssociations>a</ServiceAssociations></Service></OfferItem>` +
		`<OrderItem><Service><ServiceAssociations>b</ServiceAssociations></Service></OrderItem>` +
		`<X><ServiceAssociations>c</ServiceAssociations></X></R>`
	want := `<?xmlversion="1.0"encoding="UTF-8"standalone="no"?><R><OfferItem><Service><OfferServiceAssociation>a</OfferServiceAssociation></Service></OfferItem>` +
		`<OrderItem><Service><OrderServiceAssociation>b</OrderServiceAssociation></Service></OrderItem>` +
		`<X><Other>c</Other></X></R>`
	if got := translateString(t, m, in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestWrapEach(t *testing.T) {
	m := &mapping{WrapEach: map[string]string{"CabinCompartment/ColumnID": "SeatColumn"}}
	in := `<CabinCompartment><ColumnID>A</ColumnID><ColumnID>B</ColumnID><SeatRow><Seat><ColumnID>A</ColumnID></Seat></SeatRow></CabinCompartment>`
	want := `<?xmlversion="1.0"encoding="UTF-8"standalone="no"?><CabinCompartment><SeatColumn><ColumnID>A</ColumnID></SeatColumn><SeatColumn><ColumnID>B</ColumnID></SeatColumn><SeatRow><Seat><ColumnID>A</ColumnID></Seat></SeatRow></CabinCompartment>`
	if got := translateString(t, m, in); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// 21.3 renames SurchargeInfo to PaymentSurcharge and strips the PaymentFee prefix
// from every child. The container rename on its own produces a document that looks
// converted and is not: xmllint rejects it on the first child against
// PaymentSurchargeType, which is why the six scoped child renames are in the mapping.
func TestSurchargeInfoRename(t *testing.T) {
	ms, err := loadMappings("../translations")
	if err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"21.3", "24.1", "24.4"} {
		m := ms.byPair["19.2->"+to]
		if m == nil {
			t.Fatalf("no 19.2->%s mapping", to)
		}
		in := `<IATA_AirShoppingRS xmlns="http://www.iata.org/IATA/2015/00/2019.2/IATA_AirShoppingRS">` +
			`<Response><PaymentFunctions><PaymentSupportedMethod><SurchargeInfo>` +
			`<PaymentFeeAmountRangeMaximumAmount CurCode="USD">50.00</PaymentFeeAmountRangeMaximumAmount>` +
			`<PaymentFeeRoundingPrecisionCode>Up</PaymentFeeRoundingPrecisionCode>` +
			`</SurchargeInfo></PaymentSupportedMethod></PaymentFunctions></Response></IATA_AirShoppingRS>`
		got := translateString(t, m, in)
		for _, want := range []string{
			`<PaymentSurcharge><AmountRangeMaximumAmountCurCode="USD">50.00</AmountRangeMaximumAmount>` +
				`<RoundingPrecisionCode>Up</RoundingPrecisionCode></PaymentSurcharge>`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("19.2->%s: missing\n%s\nin\n%s", to, want, got)
			}
		}
		if strings.Contains(got, "PaymentFee") {
			t.Errorf("19.2->%s: a PaymentFee-prefixed child survived:\n%s", to, got)
		}
	}
}
