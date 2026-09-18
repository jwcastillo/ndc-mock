package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// An NDC offer identifier is frequently not opaque: it is a pipe-delimited list
// of segments, most of them base64 of plain text carrying the trip itself. For
// example:
//
//	<context fields>|GRU,NAT,2026-09-30|<fare fields>
//	IF=GRU,NAT,1234,XX,XX,2026-09-30T09:10:00,...
//	PR=USD/351
//	PX=1/0/0
//
// Clients parse these to build the next request in the flow, so the mock has to
// re-encode them to agree with what was asked instead of echoing the reference.
type segment struct {
	text   string // plain text, already decoded when the segment was base64
	isB64  bool
	suffix string // kept verbatim after the base64, as in "UFg9MS8wLzA=~0"
}

type offerID struct{ segs []segment }

var (
	rePrice   = regexp.MustCompile(`^PR=([A-Z]{3})/(\d+)(\.\d+)?$`)
	reCarrSeg = regexp.MustCompile(`\b([A-Z0-9]{2})\b`)
)

func parseOfferID(v string) offerID {
	var o offerID
	for _, s := range strings.Split(v, "|") {
		if t, ok := decodeText(s); ok {
			o.segs = append(o.segs, segment{text: t, isB64: true})
			continue
		}
		// Some providers append a marker after the base64 ("...=~0").
		if b, suf, found := strings.Cut(s, "~"); found {
			if t, ok := decodeText(b); ok {
				o.segs = append(o.segs, segment{text: t, isB64: true, suffix: "~" + suf})
				continue
			}
		}
		o.segs = append(o.segs, segment{text: s})
	}
	return o
}

func decodeText(s string) (string, bool) {
	d, err := base64.StdEncoding.DecodeString(s)
	if err != nil || !utf8.Valid(d) || !isPrintable(d) {
		return "", false
	}
	return string(d), true
}

func isPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\t' {
			return false
		}
	}
	return len(b) > 0
}

// render re-encodes the identifier, applying reps (old/new pairs) and, when an
// airline profile is given, swapping the carrier code and converting the fare
// into that carrier's currency. It writes into dst to avoid allocating per offer.
//
// cycle > 0 means this offer block is being recycled to reach the requested
// count; the fare is nudged so no two identifiers come out identical.
func (o offerID) render(dst []byte, reps *strings.Replacer, cycle int, refCarrier string, air *airline) []byte {
	for i, s := range o.segs {
		if i > 0 {
			dst = append(dst, '|')
		}
		t := reps.Replace(s.text)
		if air != nil && refCarrier != "" && air.Code != refCarrier {
			t = swapCarrier(t, refCarrier, air.Code)
		}
		if g := rePrice.FindStringSubmatch(t); g != nil {
			cur, amt := g[1], atoi(g[2])
			if air != nil {
				cur, amt = air.convert(cur, amt)
			}
			// Decimals are kept as they came: only the whole units move.
			t = "PR=" + cur + "/" + strconv.Itoa(amt+cycle*1000) + g[3]
		}
		if s.isB64 {
			dst = base64.StdEncoding.AppendEncode(dst, []byte(t))
		} else {
			dst = append(dst, t...)
		}
		dst = append(dst, s.suffix...)
	}
	return dst
}

// swapCarrier replaces standalone occurrences of the reference carrier code so
// that "LI" inside a word like "PUBLIC" is left alone.
func swapCarrier(s, from, to string) string {
	return reCarrSeg.ReplaceAllStringFunc(s, func(m string) string {
		if m == from {
			return to
		}
		return m
	})
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// encode reassembles the identifier from its segments, re-encoding the ones
// that arrived as base64. Used where a decoded identifier has to be described
// again without the original string.
func (o offerID) encode() string {
	parts := make([]string, 0, len(o.segs))
	for _, s := range o.segs {
		if s.isB64 {
			parts = append(parts, base64.StdEncoding.EncodeToString([]byte(s.text))+s.suffix)
			continue
		}
		parts = append(parts, s.text)
	}
	return strings.Join(parts, "|")
}

// reencode rebuilds an identifier so it carries the values actually served,
// not the reference ones. The caller hands this back to price and order, so it
// has to describe the trip that was returned.
func reencode(orig string, info offerInfo) string {
	o := parseOfferID(orig)
	for i, s := range o.segs {
		t := s.text
		if g := reInfoTrip.FindStringSubmatch("|" + t + "|"); g != nil {
			trips := make([]string, 0, len(info.Legs))
			for _, l := range info.Legs {
				trips = append(trips, l.From+","+l.To+","+l.Date)
			}
			t = strings.Replace(t, g[1], strings.Join(trips, "."), 1)
		}
		if g := rePrice.FindStringSubmatch(t); g != nil {
			t = "PR=" + info.Currency + "/" + strconv.Itoa(info.Amount) + g[3]
		}
		if strings.HasPrefix(t, "PX=") {
			t = fmt.Sprintf("PX=%d/%d/%d", info.ADT, info.CHD, info.INF)
		}
		o.segs[i].text = t
	}
	return o.encode()
}
