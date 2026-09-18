package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// Three modes share one code path:
//
//	mock    generate synthetically (the default)
//	proxy   forward to a real provider and return what it says
//	record  proxy, and write each response to disk
//
// Record is how synthetic data gets made: run the real traffic through once,
// and the captured responses become the reference files the mock serves from.
// It also means a capture never needs a bespoke script - whatever a client
// already sends is what gets recorded.
type proxyMode struct {
	upstream string
	recordTo string
	authHdr  []string // header names forwarded verbatim from the caller
	seq      atomic.Uint64
}

var prox *proxyMode

func newProxyMode(upstream, recordTo string, forward []string) *proxyMode {
	if upstream == "" {
		return nil
	}
	p := &proxyMode{
		upstream: strings.TrimSuffix(upstream, "/"),
		recordTo: recordTo,
		authHdr:  forward,
	}
	if recordTo != "" {
		if err := os.MkdirAll(recordTo, 0o755); err != nil {
			log.Fatalf("record directory: %v", err)
		}
	}
	return p
}

// handle forwards the request upstream. It returns false when the caller should
// fall back to generating a response, so a partial outage upstream degrades to
// synthetic rather than failing the load test.
func (p *proxyMode) handle(w http.ResponseWriter, r *http.Request, body []byte, op string) bool {
	url := p.upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("proxy: building request: %v", err)
		return false
	}
	// Credentials belong to the caller. The mock forwards them and never reads,
	// logs or stores them.
	for _, h := range p.authHdr {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Content-Type", orDefault(r.Header.Get("Content-Type"), "application/xml"))

	start := time.Now()
	resp, err := proxyClient.Do(req)
	if err != nil {
		log.Printf("proxy: %s: %v", op, err)
		return false
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		log.Printf("proxy: reading %s: %v", op, err)
		return false
	}
	log.Printf("proxy %s -> %d, %d KB, %s", op, resp.StatusCode, len(out)/1024, time.Since(start).Round(time.Millisecond))

	if p.recordTo != "" && resp.StatusCode == http.StatusOK {
		p.record(op, body, out)
	}
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Mock-Mode", "proxy")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return true
}

var proxyClient = &http.Client{Timeout: 120 * time.Second}

// Route naming reads the itinerary, not the first location in the document:
// the point of sale city appears before it and would mislabel every capture.
var reRoute = regexp.MustCompile(`<OriginDepCriteria>(?s).*?<IATA_LocationCode>([A-Z]{3})</IATA_LocationCode>`)
var reDest = regexp.MustCompile(`<DestArrivalCriteria>(?s).*?<IATA_LocationCode>([A-Z]{3})</IATA_LocationCode>`)

// record writes the exchange to disk, named after the operation and the route
// it carried, so the files are recognisable when they later become references.
func (p *proxyMode) record(op string, rq, rs []byte) {
	name := strings.ReplaceAll(op, "/", "-")
	o := reRoute.FindSubmatch(rq)
	d := reDest.FindSubmatch(rq)
	if o != nil && d != nil {
		name += "-" + string(o[1]) + "-" + string(d[1])
	}
	// Numbering restarts with every process, so a name already on disk from an
	// earlier run is skipped rather than overwritten: captures are hard to redo.
	for {
		base := filepath.Join(p.recordTo, fmt.Sprintf("%s-%03d", name, p.seq.Add(1)))
		f, err := os.OpenFile(base+"-rs.xml", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			log.Printf("record: %v", err)
			return
		}
		_, err = f.Write(rs)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			log.Printf("record: %v", err)
			return
		}
		_ = os.WriteFile(base+"-rq.xml", rq, 0o644)
		return
	}
}
