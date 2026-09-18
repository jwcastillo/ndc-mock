package main

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A chunk is a span of the response sliced at every point that varies.
// Rendering concatenates parts[i] + the value of slot i into one buffer:
// writing the document piecemeal to the socket costs 4x (measured).
type chunk struct {
	parts [][]byte
	slots []int
}

// The reference response is split into three zones so offers can be trimmed or
// multiplied: the prefix (DataLists) is fixed, each offer block is the unit that
// repeats, and the suffix closes the document.
type template struct {
	prefix, suffix chunk
	blocks         []chunk
	offers         []offerID
	dates          []string // distinct dates in the reference, sorted
	ref            refValues
	roundTrip      bool
	ver            *version
	size           int
	legs           []leg    // the reference itinerary
	airports       []string // its distinct airports; each is a slot
	shape          string   // pattern(legs)
	// Set when the template was produced by translating another generation's
	// reference at startup.
	xlate *mapping
	rep   report
}

// Slot layout: one per reference airport + one per distinct date + one per
// offer id.
func (t *template) slotDate(i int) int  { return len(t.airports) + i }
func (t *template) slotOffer(i int) int { return len(t.airports) + len(t.dates) + i }

type refValues struct {
	origin, dest     string
	depDate, retDate time.Time
	legDates         []time.Time
	pax              string
	carrier          string
	currency         string
}

const dateFmt = "2006-01-02"

var (
	reDate   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	rePax    = regexp.MustCompile(`PX=\d+/\d+/\d+`)
	reMarker = regexp.MustCompile("\x00(\\d+)\x00")
	// The trip segment carries the itinerary, one "AAA,BBB,2026-09-30" per leg
	// joined by dots: one way, a return, or every leg of a multi-city.
	reTrip = regexp.MustCompile(`\|([A-Z]{3},[A-Z]{3},\d{4}-\d{2}-\d{2}(?:\.[A-Z]{3},[A-Z]{3},\d{4}-\d{2}-\d{2})*)\|`)
	// Flight segment: IF=ORIG,DEST,FLTNUM,MKT,OPR,...
	reFlight = regexp.MustCompile(`IF=[A-Z]{3},[A-Z]{3},\d+,([A-Z0-9]{2}),`)
)

// uniqueLegIDs renames repeated DatedOperatingLegIDs (ID_2, ID_3...). A
// provider can repeat the same operated leg inside every PaxSegment that
// flies it, which breaks the schema's uniqueness key; nothing references a
// 19.2 leg by ID, so renaming the repeats changes no meaning.
func uniqueLegIDs(raw []byte) ([]byte, int) {
	seen := map[string]int{}
	renamed := 0
	out := reLegID.ReplaceAllFunc(raw, func(m []byte) []byte {
		id := string(reLegID.FindSubmatch(m)[1])
		seen[id]++
		if seen[id] == 1 {
			return m
		}
		renamed++
		return fmt.Appendf(nil, "<DatedOperatingLegID>%s_%d</DatedOperatingLegID>", id, seen[id])
	})
	return out, renamed
}

var reLegID = regexp.MustCompile(`<DatedOperatingLegID>([^<]+)</DatedOperatingLegID>`)

func newTemplate(raw []byte, ver *version) (*template, error) {
	t := &template{size: len(raw), ver: ver}

	// Identifier elements leave the document and become slots, so the base64
	// they carry takes no part in the plain-text substitutions that follow.
	idAlt := strings.Join(ver.IDElems, "|")
	reIDEl := regexp.MustCompile(`(<(?:` + idAlt + `)>)([^<]+)(</(?:` + idAlt + `)>)`)
	doc := reIDEl.ReplaceAllFunc(raw, func(m []byte) []byte {
		g := reIDEl.FindSubmatch(m)
		t.offers = append(t.offers, parseOfferID(string(g[2])))
		return fmt.Appendf(nil, "%s\x00o%d\x00%s", g[1], len(t.offers)-1, g[3])
	})
	if len(t.offers) == 0 {
		return nil, fmt.Errorf("reference response carries no %s element", ver.IDElems[0])
	}

	// Route, dates, passenger mix and carrier are read back out of the first
	// identifier, so the template configures itself from any reference file.
	for _, s := range t.offers[0].segs {
		if g := reTrip.FindStringSubmatch("|" + s.text + "|"); g != nil && t.legs == nil {
			t.legs = parseLegs(g[1])
		}
		if m := rePax.FindString(s.text); m != "" {
			t.ref.pax = m
		}
		if g := reFlight.FindStringSubmatch(s.text); g != nil {
			t.ref.carrier = g[1]
		}
		if g := rePrice.FindStringSubmatch(s.text); g != nil {
			t.ref.currency = g[1]
		}
	}
	if len(t.legs) == 0 {
		return nil, fmt.Errorf("could not derive the reference itinerary from the first identifier")
	}
	t.airports, t.shape = airports(t.legs), pattern(t.legs)
	t.roundTrip = t.shape == "0-1,1-0"
	for _, l := range t.legs {
		d, err := time.Parse(dateFmt, l.Date)
		if err != nil {
			return nil, fmt.Errorf("reference leg date %q: %w", l.Date, err)
		}
		t.ref.legDates = append(t.ref.legDates, d)
	}
	t.ref.origin, t.ref.dest = t.legs[0].From, t.legs[0].To
	t.ref.depDate, t.ref.retDate = t.ref.legDates[0], t.ref.legDates[len(t.legs)-1]

	// Every distinct date becomes its own slot. They cannot be substituted as
	// fixed text: a next-day arrival has to stay next-day when the requested
	// date moves.
	t.dates = uniqSorted(reDate.FindAllString(string(doc), -1))
	reps := make([]string, 0, 2*len(t.dates))
	for i, d := range t.dates {
		reps = append(reps, d, fmt.Sprintf("\x00d%d\x00", i))
	}
	doc = []byte(strings.NewReplacer(reps...).Replace(string(doc)))
	airs := make([]string, 0, 2*len(t.airports))
	for i, a := range t.airports {
		airs = append(airs, a, fmt.Sprintf("\x00%d\x00", i))
	}
	doc = []byte(strings.NewReplacer(airs...).Replace(string(doc)))

	// Lettered markers become numeric indices now that the date count is known.
	doc = renumber(doc, "d", func(i int) int { return t.slotDate(i) })
	doc = renumber(doc, "o", func(i int) int { return t.slotOffer(i) })

	// Split into prefix / offer blocks / suffix.
	open := []byte("<" + ver.OfferElem + ">")
	closeTag := []byte("</" + ver.OfferElem + ">")
	first, last := bytes.Index(doc, open), bytes.LastIndex(doc, closeTag)
	if first < 0 || last < 0 {
		return nil, fmt.Errorf("no <%s> blocks in the reference response", ver.OfferElem)
	}
	last += len(closeTag)
	t.prefix, t.suffix = newChunk(doc[:first]), newChunk(doc[last:])
	for rest := doc[first:last]; len(rest) > 0; {
		i := bytes.Index(rest, closeTag)
		if i < 0 {
			break
		}
		i += len(closeTag)
		t.blocks = append(t.blocks, newChunk(rest[:i]))
		rest = rest[i:]
	}
	if len(t.blocks) == 0 {
		return nil, fmt.Errorf("could not slice the <%s> blocks", ver.OfferElem)
	}
	return t, nil
}

func renumber(doc []byte, letter string, to func(int) int) []byte {
	re := regexp.MustCompile("\x00" + letter + "(\\d+)\x00")
	return []byte(re.ReplaceAllStringFunc(string(doc), func(m string) string {
		var i int
		fmt.Sscanf(m, "\x00"+letter+"%d\x00", &i)
		return fmt.Sprintf("\x00%d\x00", to(i))
	}))
}

func newChunk(doc []byte) chunk {
	var c chunk
	last := 0
	for _, m := range reMarker.FindAllSubmatchIndex(doc, -1) {
		c.parts = append(c.parts, doc[last:m[0]])
		var n int
		fmt.Sscanf(string(doc[m[2]:m[3]]), "%d", &n)
		c.slots = append(c.slots, n)
		last = m[1]
	}
	c.parts = append(c.parts, doc[last:])
	return c
}

func uniqSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// shiftDates moves every date in the reference while preserving its offset.
// Each one belongs to the leg whose reference date is nearest and moves by
// that leg's shift, so a next-day arrival stays next-day on every leg.
func (t *template) shiftDates(q query) []string {
	legs := q.Legs
	if len(legs) == 0 {
		legs = []leg{{q.Origin, q.Dest, q.DepDate}}
	}
	deltas := make([]time.Duration, len(t.ref.legDates))
	for i, ref := range t.ref.legDates {
		want := legs[min(i, len(legs)-1)].Date
		if d, err := time.Parse(dateFmt, want); err == nil {
			deltas[i] = d.Sub(ref)
		} else {
			return t.dates
		}
	}
	out := make([]string, len(t.dates))
	for i, s := range t.dates {
		d, err := time.Parse(dateFmt, s)
		if err != nil {
			out[i] = s
			continue
		}
		near := 0
		for j, ref := range t.ref.legDates {
			if absDur(d.Sub(ref)) < absDur(d.Sub(t.ref.legDates[near])) {
				near = j
			}
		}
		out[i] = d.Add(deltas[near]).Format(dateFmt)
	}
	return out
}

// airportsFor maps the reference airports onto the request's, position by
// position in the shared itinerary shape.
func (t *template) airportsFor(q query) []string {
	want := airports(q.Legs)
	if len(want) == 0 {
		want = []string{q.Origin, q.Dest}
	}
	out := make([]string, len(t.airports))
	for i, a := range t.airports {
		out[i] = a
		if i < len(want) {
			out[i] = want[i]
		}
	}
	return out
}

// parseLegs reads "AAA,BBB,2026-09-30.BBB,CCC,2026-10-07" into legs.
func parseLegs(trip string) []leg {
	var out []leg
	for _, p := range strings.Split(trip, ".") {
		f := strings.Split(p, ",")
		if len(f) == 3 {
			out = append(out, leg{f[0], f[1], f[2]})
		}
	}
	return out
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// NaturalOffers is how many offers the reference response carries untouched.
func (t *template) NaturalOffers() int { return len(t.blocks) }

// render builds the response with n offers for the given request and carrier.
// When n exceeds what the reference holds, blocks are recycled; each cycle
// carries a price delta so no two offers come out identical.
func (t *template) render(dst []byte, q query, n int, air *airline) []byte {
	if n <= 0 {
		n = len(t.blocks)
	}
	dates := t.shiftDates(q)
	airs := t.airportsFor(q)
	vals := make([]string, t.slotOffer(0))
	copy(vals, airs)
	copy(vals[len(airs):], dates)

	// Identifiers are rewritten with the same substitutions, applied to their
	// decoded plain text.
	reps := make([]string, 0, 2*len(t.dates)+6)
	for i, a := range t.airports {
		reps = append(reps, a, airs[i])
	}
	reps = append(reps, t.ref.pax, q.paxSeg())
	for i, d := range t.dates {
		reps = append(reps, d, dates[i])
	}
	rep := strings.NewReplacer(reps...)

	var idbuf []byte
	emit := func(c chunk, cycle int) {
		for i, p := range c.parts {
			dst = append(dst, p...)
			if i >= len(c.slots) {
				continue
			}
			if s := c.slots[i]; s < len(vals) {
				dst = append(dst, vals[s]...)
			} else {
				idbuf = t.offers[s-len(vals)].render(idbuf[:0], rep, cycle, t.ref.carrier, air)
				dst = append(dst, idbuf...)
			}
		}
	}
	emit(t.prefix, 0)
	for i := 0; i < n; i++ {
		emit(t.blocks[i%len(t.blocks)], i/len(t.blocks))
	}
	emit(t.suffix, 0)
	return dst
}
