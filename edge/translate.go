package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Translation between IATA generations runs once, at startup, over the
// reference response: the result becomes an ordinary template of the target
// generation, so serving it costs nothing per request.
//
// Mappings are data derived from the official schemas (tools/compare-paths.py,
// tools/validate-translation.sh), never guessed. What a mapping can say:
// renames and drops (optionally scoped to a parent), wrappers, value codes,
// namespaces, alphabetical child order, and named structural transforms that
// live in code because they build new lists and references.
type mapping struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Namespaces are replaced on the root's namespace declarations.
	Namespaces map[string]string `json:"namespaces"`
	// Declare adds prefix declarations to the root.
	Declare map[string]string `json:"declare"`
	// MessageLayout is the 21.3+ namespace split, for any message: the root and
	// its direct children in the message namespace (prefix m:), everything
	// below in the common-types one, which becomes the default.
	MessageLayout *struct {
		Message string `json:"message"`
		Types   string `json:"types"`
	} `json:"messageLayout"`
	// Rename maps a From element name to its To name. A key with slashes is a
	// path suffix, "OrderItem/Service/ServiceAssociations", matched on From
	// names; the longest matching key wins.
	Rename map[string]string `json:"rename"`
	// Drop removes elements with their subtree; "Parent/Child" is scoped.
	Drop []string `json:"drop"`
	// Wrap gathers every Child of Parent into one new wrapper element,
	// "Parent/Child": "Wrapper", in From names. A wrapper path "Outer/Inner"
	// nests: 21.3 put a service definition's segment references inside
	// OfferFlightAssociations/PaxSegmentReferences.
	Wrap map[string]string `json:"wrap"`
	// WrapEach puts every Child of Parent in its own wrapper, "Parent/Child":
	// "Wrapper": 21.3 made each seat-map column a SeatColumn.
	WrapEach map[string]string `json:"wrapEach"`
	// Hoist moves Child of Parent up to the nearest ancestor named Ancestor,
	// "Parent/Child": "Ancestor", dropping repeats: 21.3 moved FareRule's
	// PenaltyRefID up to FareDetail.
	Hoist map[string]string `json:"hoist"`
	// Values rewrites closed code lists: element -> {old: new}.
	Values map[string]map[string]string `json:"values"`
	// Structural names transforms implemented in code (see structural).
	Structural []string `json:"structural"`
	// SortChildren orders every element's children alphabetically, ignoring
	// case, below the root. From 21.3 every sequence in the schema is in that
	// order, so this reproduces the target order without per-type data.
	SortChildren bool `json:"sortChildren"`
	// Identical lists names verified to mean the same thing in both, so they
	// count as covered rather than as unverified pass-through.
	Identical []string `json:"identical"`
	// Notes records what the mapping cannot express, and the evidence.
	Notes []string `json:"notes"`

	covered map[string]bool
}

type mappingSet struct {
	byPair map[string]*mapping // "19.2->24.1"
}

func loadMappings(dir string) (*mappingSet, error) {
	ms := &mappingSet{byPair: map[string]*mapping{}}
	if dir == "" {
		return ms, nil
	}
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range entries {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		m := &mapping{}
		if err := json.Unmarshal(b, m); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if m.From == "" || m.To == "" {
			return nil, fmt.Errorf("%s: mapping declares no from/to", f)
		}
		for _, s := range m.Structural {
			if structural[s] == nil {
				return nil, fmt.Errorf("%s: unknown structural transform %q", f, s)
			}
		}
		m.covered = map[string]bool{}
		for k, v := range m.Rename {
			m.covered[lastSeg(k)], m.covered[local(v)] = true, true
		}
		for _, k := range m.Drop {
			m.covered[lastSeg(k)] = true
		}
		for k, v := range m.Wrap {
			m.covered[lastSeg(k)] = true
			for _, w := range strings.Split(v, "/") {
				m.covered[w] = true
			}
		}
		for k, v := range m.WrapEach {
			m.covered[lastSeg(k)], m.covered[v] = true, true
		}
		for k := range m.Hoist {
			m.covered[lastSeg(k)] = true
		}
		for _, s := range m.Structural {
			for _, n := range structuralNames[s] {
				m.covered[n] = true
			}
		}
		for _, k := range m.Identical {
			m.covered[k] = true
		}
		ms.byPair[m.From+"->"+m.To] = m
	}
	return ms, nil
}

func (ms *mappingSet) find(from, to string) *mapping {
	if ms == nil {
		return nil
	}
	return ms.byPair[from+"->"+to]
}

// usable reports whether the mapping can actually change anything. An empty one
// exists to record that a generation has no source yet; using it would serve
// an untouched document while claiming a translation happened, which is worse
// than admitting the generation cannot be produced.
func (m *mapping) usable() bool {
	return m != nil && (len(m.Namespaces) > 0 || len(m.Rename) > 0 || len(m.Drop) > 0 ||
		len(m.Declare) > 0 || len(m.Wrap) > 0 || len(m.WrapEach) > 0 || len(m.Hoist) > 0 || len(m.Structural) > 0 ||
		m.MessageLayout != nil)
}

func (ms *mappingSet) pairs() []string {
	var out []string
	for k := range ms.byPair {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// report describes what a translation actually managed to do.
type report struct {
	Covered    int
	Unverified int
	Samples    []string
}

// Coverage is the share of distinct element names the mapping accounts for.
// It counts names, not validity: tools/validate-translation.sh is the stricter
// measure.
func (r report) Coverage() int {
	total := r.Covered + r.Unverified
	if total == 0 {
		return 100
	}
	return r.Covered * 100 / total
}

// translate rewrites a whole document from one generation to another.
func (m *mapping) translate(doc []byte) ([]byte, report, error) {
	root, err := parseTree(doc)
	if err != nil {
		return nil, report{}, err
	}
	rep := m.translateTree(root)
	// Keep the source's layout: providers send compact documents as often as
	// indented ones, and the payload size is what a load test measures.
	head := doc[:min(len(doc), 4096)]
	return root.serializeAs(bytes.Contains(head, []byte(">\n")) || bytes.Contains(head, []byte(">\r\n"))), rep, nil
}

// translateTree rewrites a tree in place.
func (m *mapping) translateTree(root *xnode) report {
	for _, s := range m.Structural {
		structural[s](root)
	}
	root.walk(func(n *xnode) {
		if codes := m.Values[n.name]; codes != nil {
			if v, ok := codes[strings.TrimSpace(n.text)]; ok {
				n.text = v
			}
		}
	})
	for k, ancestor := range m.Hoist {
		parent, child, _ := strings.Cut(k, "/")
		root.hoist(parent, child, ancestor, nil)
	}
	for k, wrapper := range m.Wrap {
		parent, child, _ := strings.Cut(k, "/")
		root.walk(func(n *xnode) {
			if n.name == parent {
				n.wrap(child, wrapper)
			}
		})
	}
	for k, wrapper := range m.WrapEach {
		parent, child, _ := strings.Cut(k, "/")
		root.walk(func(n *xnode) {
			if n.name != parent {
				return
			}
			for i, c := range n.kids {
				if c.name == child {
					n.kids[i] = node(wrapper, c)
				}
			}
		})
	}
	for _, d := range m.Drop {
		parent, child, scoped := strings.Cut(d, "/")
		if !scoped {
			parent, child = "", d
		}
		root.walk(func(n *xnode) {
			if parent == "" || n.name == parent {
				n.kids = removeKids(n.kids, child)
			}
		})
	}
	// Renames match From names, so every decision is taken before any is applied.
	longest := 1
	for k := range m.Rename {
		longest = max(longest, strings.Count(k, "/")+1)
	}
	renames := map[*xnode]string{}
	var path []string
	var mark func(n *xnode)
	mark = func(n *xnode) {
		path = append(path, n.name)
		for k := min(longest, len(path)); k >= 1; k-- {
			if to, ok := m.Rename[strings.Join(path[len(path)-k:], "/")]; ok {
				renames[n] = to
				break
			}
		}
		for _, c := range n.kids {
			mark(c)
		}
		path = path[:len(path)-1]
	}
	mark(root)
	for n, to := range renames {
		n.name = to
	}
	for i, a := range root.attrs {
		if to, ok := m.Namespaces[a.Value]; ok && (a.Name.Space == "xmlns" || a.Name.Space == "" && a.Name.Local == "xmlns") {
			root.attrs[i].Value = to
		}
	}
	if l := m.MessageLayout; l != nil {
		root.name = "m:" + local(root.name)
		for _, k := range root.kids {
			k.name = "m:" + local(k.name)
		}
		kept := root.attrs[:0]
		for _, a := range root.attrs {
			if a.Name.Space != "xmlns" && !(a.Name.Space == "" && a.Name.Local == "xmlns") {
				kept = append(kept, a)
			}
		}
		root.attrs = append(kept,
			xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: l.Types},
			xml.Attr{Name: xml.Name{Space: "xmlns", Local: "m"}, Value: l.Message})
	}
	prefixes := make([]string, 0, len(m.Declare))
	for p := range m.Declare {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		root.attrs = append(root.attrs, xml.Attr{Name: xml.Name{Space: "xmlns", Local: p}, Value: m.Declare[p]})
	}
	if m.SortChildren {
		root.sortBelowRoot()
	}

	var rep report
	seen := map[string]bool{}
	root.walk(func(n *xnode) {
		name := local(n.name)
		if seen[name] {
			return
		}
		seen[name] = true
		if m.covered[name] {
			rep.Covered++
			return
		}
		rep.Unverified++
		if len(rep.Samples) < 12 {
			rep.Samples = append(rep.Samples, name)
		}
	})
	return rep
}

// sortBelowRoot orders every element's children alphabetically, ignoring case,
// leaving the root's own children (Error/Response first) as they are.
func (root *xnode) sortBelowRoot() {
	for _, k := range root.kids {
		k.walk(func(n *xnode) {
			sort.SliceStable(n.kids, func(i, j int) bool {
				return strings.ToLower(local(n.kids[i].name)) < strings.ToLower(local(n.kids[j].name))
			})
		})
	}
}

func (m *mapping) describe() string { return strings.Join(m.Notes, "; ") }

func local(name string) string {
	if _, l, ok := strings.Cut(name, ":"); ok {
		return l
	}
	return name
}

func lastSeg(s string) string { return s[strings.LastIndexByte(s, '/')+1:] }

// --- tree -------------------------------------------------------------------

// xnode is the minimal tree NDC documents need: no mixed content, so an
// element holds either children or text.
type xnode struct {
	name  string // as written, prefix included
	attrs []xml.Attr
	kids  []*xnode
	text  string
}

func parseTree(doc []byte) (*xnode, error) {
	d := xml.NewDecoder(bytes.NewReader(doc))
	var stack []*xnode
	var root *xnode
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &xnode{name: qname(t.Name), attrs: append([]xml.Attr(nil), t.Attr...)}
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.kids = append(top.kids, n)
			} else {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, fmt.Errorf("unbalanced </%s>", qname(t.Name))
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(t)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("no root element")
	}
	return root, nil
}

func qname(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

func (n *xnode) walk(fn func(*xnode)) {
	fn(n)
	for _, k := range n.kids {
		k.walk(fn)
	}
}

func (n *xnode) child(name string) *xnode {
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}
	return nil
}

func (n *xnode) childText(name string) string {
	if c := n.child(name); c != nil {
		return strings.TrimSpace(c.text)
	}
	return ""
}

func (n *xnode) wrap(child, wrapper string) {
	var out []*xnode
	var w *xnode
	for _, k := range n.kids {
		if k.name != child {
			out = append(out, k)
			continue
		}
		if w == nil {
			names := strings.Split(wrapper, "/")
			outer := &xnode{name: names[0]}
			w = outer
			for _, n := range names[1:] {
				inner := &xnode{name: n}
				w.kids = append(w.kids, inner)
				w = inner
			}
			out = append(out, outer)
		}
		w.kids = append(w.kids, k)
	}
	n.kids = out
}

// hoist moves every child of a parent up to the nearest ancestor of the given
// name, skipping values that ancestor already holds.
func (n *xnode) hoist(parent, child, ancestor string, up []*xnode) {
	up = append(up, n)
	for _, k := range n.kids {
		k.hoist(parent, child, ancestor, up)
	}
	if n.name != parent {
		return
	}
	var target *xnode
	for i := len(up) - 2; i >= 0 && target == nil; i-- {
		if up[i].name == ancestor {
			target = up[i]
		}
	}
	if target == nil {
		return
	}
	for _, k := range kidsNamed(n, child) {
		dup := false
		for _, t := range kidsNamed(target, child) {
			dup = dup || strings.TrimSpace(t.text) == strings.TrimSpace(k.text)
		}
		if !dup {
			target.kids = append(target.kids, k)
		}
	}
	n.kids = removeKids(n.kids, child)
}

func removeKids(kids []*xnode, name string) []*xnode {
	out := kids[:0]
	for _, k := range kids {
		if k.name != name {
			out = append(out, k)
		}
	}
	return out
}

func leaf(name, text string) *xnode { return &xnode{name: name, text: text} }

// serialize writes the tree indented four spaces.
func (n *xnode) serialize() []byte { return n.serializeAs(true) }

// serializeAs writes the tree indented, or compact with no whitespace between
// elements.
func (n *xnode) serializeAs(indent bool) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="no"?>` + "\n")
	n.write(&b, 0, indent)
	return b.Bytes()
}

func (n *xnode) write(b *bytes.Buffer, depth int, indent bool) {
	pad, nl := "", ""
	if indent {
		pad, nl = strings.Repeat("    ", depth), "\n"
	}
	b.WriteString(pad + "<" + n.name)
	for _, a := range n.attrs {
		b.WriteString(" " + qname(a.Name) + `="`)
		xml.EscapeText(b, []byte(a.Value))
		b.WriteByte('"')
	}
	if len(n.kids) == 0 {
		b.WriteByte('>')
		xml.EscapeText(b, []byte(strings.TrimSpace(n.text)))
		b.WriteString("</" + n.name + ">" + nl)
		return
	}
	b.WriteString(">" + nl)
	for _, k := range n.kids {
		k.write(b, depth+1, indent)
	}
	b.WriteString(pad + "</" + n.name + ">" + nl)
}

// --- structural transforms --------------------------------------------------

var structural = map[string]func(root *xnode){
	"dated-segments":     datedSegments,
	"tz-into-datetime":   tzIntoDateTime,
	"weight-unit":        weightUnit,
	"unpriced-penalties": unpricedPenalties,
	"seat-row-number":    seatRowNumber,
	"definition-owner":   definitionOwner,
}

// structuralNames lists the From names a transform consumes and the To names it
// produces, so coverage counts them as accounted for.
var structuralNames = map[string][]string{
	"dated-segments": {
		"PaxSegment", "MarketingCarrierInfo", "OperatingCarrierInfo", "CabinType", "DatedOperatingLeg",
		"CabinTypeAssociationChoice", "SegmentCabinType", "DatedMarketingSegmentRefId",
		"DatedMarketingSegmentList", "DatedMarketingSegment", "DatedMarketingSegmentId",
		"DatedOperatingSegmentList", "DatedOperatingSegment", "DatedOperatingSegmentId",
		"DatedOperatingSegmentRefId", "DatedOperatingLegList", "DatedOperatingLegRefID",
		"DatedOperatingLegID",
	},
	"weight-unit": {"WeightUnitOfMeasurement"},
}

// definitionOwner gives every service definition an OwnerCode, which 21.3
// requires and 19.2 did not: the carrier that owns the document's offers,
// which is the carrier whose services they are.
func definitionOwner(root *xnode) {
	owner := ""
	root.walk(func(n *xnode) {
		if owner == "" && n.name == "Offer" {
			owner = n.childText("OwnerCode")
		}
	})
	if owner == "" {
		return
	}
	root.walk(func(n *xnode) {
		if n.name == "ServiceDefinition" && n.child("OwnerCode") == nil {
			n.kids = append(n.kids, leaf("OwnerCode", owner))
		}
	})
}

// seatRowNumber repeats each row's number on its seats: from 21.3 a seat in a
// seat map names its own row.
func seatRowNumber(root *xnode) {
	root.walk(func(n *xnode) {
		if n.name != "SeatRow" {
			return
		}
		row := n.childText("RowNumber")
		for _, seat := range kidsNamed(n, "Seat") {
			if seat.child("RowNumber") == nil && row != "" {
				seat.kids = append(seat.kids, leaf("RowNumber", row))
			}
		}
	})
}

// unpricedPenalties removes penalties that carry no amount, with every
// reference to them. 19.2 allowed a penalty that only said a fee applies; from
// 21.3 Price is mandatory, and inventing an amount would misstate the fare.
// What the dropped penalty said survives in the offer item's Change and Cancel
// restrictions.
func unpricedPenalties(root *xnode) {
	gone := map[string]bool{}
	root.walk(func(n *xnode) {
		if n.name != "PenaltyList" {
			return
		}
		kept := n.kids[:0]
		for _, p := range n.kids {
			if p.name == "Penalty" && p.child("Price") == nil {
				gone[p.childText("PenaltyID")] = true
				continue
			}
			kept = append(kept, p)
		}
		n.kids = kept
	})
	if len(gone) == 0 {
		return
	}
	root.walk(func(n *xnode) {
		kept := n.kids[:0]
		for _, k := range n.kids {
			if k.name == "PenaltyRefID" && gone[strings.TrimSpace(k.text)] {
				continue
			}
			if k.name == "PenaltyList" && len(k.kids) == 0 {
				continue // a list needs at least one entry
			}
			kept = append(kept, k)
		}
		n.kids = kept
	})
}

// tzIntoDateTime folds the 19.2 TimeZoneCode attribute into the value. From 21.3
// the attribute is gone and the element is a plain xs:dateTime, which carries
// an offset itself, so nothing is lost.
func tzIntoDateTime(root *xnode) {
	root.walk(func(n *xnode) {
		if n.name != "AircraftScheduledDateTime" {
			return
		}
		for i, a := range n.attrs {
			if a.Name.Local != "TimeZoneCode" {
				continue
			}
			v := strings.TrimSpace(n.text)
			if !reOffset.MatchString(v) && reOffset.MatchString("T"+a.Value) {
				n.text = v + a.Value
			}
			n.attrs = append(n.attrs[:i], n.attrs[i+1:]...)
			return
		}
	})
}

var reOffset = regexp.MustCompile(`(Z|[+-]\d{2}:\d{2})$`)

// weightUnit moves the unit off each weight measure into the element 21.3
// requires once per allowance, in UN/ECE Recommendation 20 codes.
func weightUnit(root *xnode) {
	codes := map[string]string{
		"KG": "KGM", "KGS": "KGM", "KGM": "KGM", "KILO": "KGM", "KILOS": "KGM", "KILOGRAM": "KGM", "KILOGRAMS": "KGM",
		"LB": "LBR", "LBS": "LBR", "LBR": "LBR", "POUND": "LBR", "POUNDS": "LBR",
	}
	root.walk(func(n *xnode) {
		if n.name != "WeightAllowance" {
			return
		}
		unit := ""
		for _, k := range n.kids {
			kept := k.attrs[:0]
			for _, a := range k.attrs {
				if a.Name.Local == "UnitCode" {
					unit = orDefault(unit, codes[strings.ToUpper(a.Value)])
					continue
				}
				kept = append(kept, a)
			}
			k.attrs = kept
		}
		if unit != "" && n.child("WeightUnitOfMeasurement") == nil {
			n.kids = append(n.kids, leaf("WeightUnitOfMeasurement", unit))
		}
	})
}

// datedSegments reshapes 19.2 PaxSegments into the 21.3 model. 19.2 kept the
// whole flight inside PaxSegment; from 21.3 the flight is a DatedMarketingSegment
// pointing at a DatedOperatingSegment made of DatedOperatingLegs, each in its
// own list, and PaxSegment only references it. Identifiers are derived from
// the PaxSegmentID, one to one, so they stay unique and traceable.
func datedSegments(root *xnode) {
	root.walk(func(dl *xnode) {
		if dl.name != "DataLists" {
			return
		}
		psl := dl.child("PaxSegmentList")
		if psl == nil {
			return
		}
		dms := &xnode{name: "DatedMarketingSegmentList"}
		dos := &xnode{name: "DatedOperatingSegmentList"}
		dol := &xnode{name: "DatedOperatingLegList"}
		legSeen := map[string]bool{}
		for _, ps := range psl.kids {
			id := ps.childText("PaxSegmentID")
			mkt := ps.child("MarketingCarrierInfo")
			opr := ps.child("OperatingCarrierInfo")
			dep, arr := ps.child("Dep"), ps.child("Arrival")

			seg := &xnode{name: "DatedMarketingSegment"}
			seg.kids = append(seg.kids, arr)
			seg.kids = append(seg.kids, leaf("CarrierDesigCode", mkt.childText("CarrierDesigCode")))
			if v := mkt.childText("CarrierName"); v != "" {
				seg.kids = append(seg.kids, leaf("CarrierName", v))
			}
			seg.kids = append(seg.kids,
				leaf("DatedMarketingSegmentId", "DMS_"+id),
				leaf("DatedOperatingSegmentRefId", "DOS_"+id),
				dep,
				leaf("MarketingCarrierFlightNumberText", mkt.childText("MarketingCarrierFlightNumberText")))
			if v := mkt.childText("OperationalSuffixText"); v != "" {
				seg.kids = append(seg.kids, leaf("OperationalSuffixText", v))
			}
			if c := ps.child("TicketlessInd"); c != nil {
				seg.kids = append(seg.kids, c)
			}
			dms.kids = append(dms.kids, seg)

			op := &xnode{name: "DatedOperatingSegment"}
			carrier := mkt.childText("CarrierDesigCode")
			if opr != nil && opr.childText("CarrierDesigCode") != "" {
				carrier = opr.childText("CarrierDesigCode")
			}
			op.kids = append(op.kids, leaf("CarrierDesigCode", carrier))
			for n, leg := range kidsNamed(ps, "DatedOperatingLeg") {
				legID := leg.childText("DatedOperatingLegID")
				if legID == "" {
					legID = fmt.Sprintf("LEG_%s_%d", id, n+1)
					leg.kids = append(leg.kids, leaf("DatedOperatingLegID", legID))
				}
				// 21.3 requires a location at both ends of a leg; 19.2 did not.
				fillLocation(leg, "Dep", dep)
				fillLocation(leg, "Arrival", arr)
				// OnGroundDuration was a time of day in 19.2 and is a duration now.
				leg.kids = removeKids(leg.kids, "OnGroundDuration")
				// One leg flown under several segments is listed once.
				if !legSeen[legID] {
					legSeen[legID] = true
					dol.kids = append(dol.kids, leg)
				}
				op.kids = append(op.kids, leaf("DatedOperatingLegRefID", legID))
			}
			op.kids = append(op.kids, leaf("DatedOperatingSegmentId", "DOS_"+id))
			if opr != nil {
				if c := opr.child("DisclosureRefID"); c != nil {
					op.kids = append(op.kids, c)
				}
			}
			for _, name := range []string{"Duration"} {
				if c := ps.child(name); c != nil {
					op.kids = append(op.kids, c)
				}
			}
			if opr != nil {
				for _, name := range []string{"OperatingCarrierFlightNumberText", "OperationalSuffixText"} {
					if c := opr.child(name); c != nil {
						op.kids = append(op.kids, c)
					}
				}
			}
			for _, name := range []string{"SecureFlightInd", "SegmentTypeCode"} {
				if c := ps.child(name); c != nil {
					op.kids = append(op.kids, c)
				}
			}
			dos.kids = append(dos.kids, op)

			// What PaxSegment keeps.
			keep := []*xnode{}
			if c := ps.child("CabinType"); c != nil {
				c.name = "SegmentCabinType"
				keep = append(keep, &xnode{name: "CabinTypeAssociationChoice", kids: []*xnode{c}})
			}
			keep = append(keep, leaf("DatedMarketingSegmentRefId", "DMS_"+id))
			rbd := ps.child("MarketingCarrierRBD_Code")
			if rbd == nil && mkt.childText("RBD_Code") != "" {
				rbd = leaf("MarketingCarrierRBD_Code", mkt.childText("RBD_Code"))
			}
			if rbd != nil {
				keep = append(keep, rbd)
			}
			if c := ps.child("OperatingCarrierRBD_Code"); c != nil {
				keep = append(keep, c)
			}
			keep = append(keep, leaf("PaxSegmentID", id))
			ps.kids = keep
		}
		for _, l := range []*xnode{dms, dos, dol} {
			if len(l.kids) > 0 {
				dl.kids = append(dl.kids, l)
			}
		}
	})
}

func kidsNamed(n *xnode, name string) []*xnode {
	var out []*xnode
	for _, k := range n.kids {
		if k.name == name {
			out = append(out, k)
		}
	}
	return out
}

func fillLocation(leg *xnode, end string, from *xnode) {
	p := leg.child(end)
	if p == nil {
		p = &xnode{name: end}
		leg.kids = append(leg.kids, p)
	}
	if p.child("IATA_LocationCode") == nil && from != nil && from.childText("IATA_LocationCode") != "" {
		p.kids = append(p.kids, leaf("IATA_LocationCode", from.childText("IATA_LocationCode")))
	}
}
