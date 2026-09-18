package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	addr      = flag.String("addr", ":8090", "listen address")
	stubs     = flag.String("stubs", "", "directory holding reference responses, one subdirectory per version")
	cfgPath   = flag.String("config", "", "per-route offer count and latency (routes.json)")
	verPath   = flag.String("versions", "", "IATA version profiles (versions.json)")
	airPath   = flag.String("airlines", "", "carrier profiles (airlines.json)")
	delay     = flag.Duration("delay", 0, "default latency when no -config is given")
	offers    = flag.Int("offers", 0, "default offer count when no -config is given; 0 = as in the reference")
	defAir    = flag.String("airline", "", "default carrier code; empty keeps the reference carrier")
	flatVer   = flag.String("flat-version", "v192", "which version a flat -stubs layout (no per-version subdirectory) belongs to")
	langHdr   = flag.String("lang-header", "Accept-Language", "request header carrying the response language; providers often use a vendor-specific one")
	orderTTL  = flag.Duration("order-ttl", time.Hour, "how long a created order stays retrievable")
	orderMax  = flag.Int("order-max", 50000, "maximum orders held in memory; the oldest is evicted past this")
	maxOffers = flag.Int("max-offers", 5000, "hard ceiling on offers per response; caps memory amplification from X-Mock-Offers")
	maxDelay  = flag.Duration("max-delay", 2*time.Minute, "hard ceiling on any simulated latency, header or config, so a caller cannot pin connections open; high enough for a slow order operation's p99")
	advertise = flag.String("advertise", os.Getenv("NDC_MOCK_ADVERTISE"), "this replica's host:port as peers reach it; set with -peers to run several replicas")
	peers     = flag.String("peers", os.Getenv("NDC_MOCK_PEERS"), "DNS name resolving to every replica (a headless Service); orders are forwarded only to these")
	transDir  = flag.String("translations", "", "directory of version-translation mappings (translations/)")
	maps      *mappingSet
	upstream  = flag.String("proxy", "", "forward to this NDC provider instead of generating; empty = synthetic")
	recordTo  = flag.String("record", "", "with -proxy, write each upstream response to this directory")
	fwdHdrs   = flag.String("forward-headers", "Authorization,X-Api-Key,Accept-Language", "comma-separated headers forwarded verbatim upstream")
	opsPath   = flag.String("operations", "", "route layout under /ndc/<version>/ (JSON {\"routes\": {path: handler}}); empty = one path per IATA message")
	tokenPath = flag.String("token-path", "/oauth/token", "path of the stand-in OAuth client-credentials endpoint")
	cfg       *config
	airs      *airlineSet
	vers      *versionSet
	templates = map[string]*template{} // key: "<version>|<trip>:<lang>"
	bufs      = sync.Pool{New: func() any { b := make([]byte, 0, 8<<20); return &b }}
)

// handlers are the operations the mock implements, by IATA message. None but
// AirShopping needs a reference response: the offer identifier describes the
// trip and the order store carries the rest.
var handlers = map[string]http.HandlerFunc{
	"airshopping":      airshopping,
	"offerprice":       offerPrice,
	"ordercreate":      orderCreate,
	"orderretrieve":    orderRetrieve,
	"orderchange":      orderChange,
	"ordercancel":      orderCancel,
	"orderreshop":      orderReshop,
	"servicelist":      serviceList,
	"seatavailability": seatAvailability,
	"installments":     installments,
}

// routes maps each path under /ndc/<version>/ to a handler. The default is one
// path per IATA message; -operations replaces it with a provider's own layout
// (several paths per message, extensions such as installments).
var routes = map[string]string{
	"airshopping": "airshopping", "offerprice": "offerprice",
	"ordercreate": "ordercreate", "orderretrieve": "orderretrieve", "orderchange": "orderchange",
	"ordercancel": "ordercancel", "orderreshop": "orderreshop",
	"servicelist": "servicelist", "seatavailability": "seatavailability",
}

// loadRoutes reads {"routes": {"<path>": "<handler>"}} and checks every
// handler exists.
func loadRoutes(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f struct {
		Routes map[string]string `json:"routes"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for p, h := range f.Routes {
		if handlers[h] == nil {
			return fmt.Errorf("%s: route %s names unknown handler %q", path, p, h)
		}
	}
	if len(f.Routes) == 0 {
		return fmt.Errorf("%s: no routes", path)
	}
	routes = f.Routes
	return nil
}

func main() {
	flag.Parse()
	if *stubs == "" {
		log.Fatal("missing -stubs: point it at a directory of reference responses")
	}

	var err error
	if vers, err = loadVersions(orDefault(*verPath, "../versions.json")); err != nil {
		log.Fatal(err)
	}
	if airs, err = loadAirlines(orDefault(*airPath, "../airlines.json")); err != nil {
		log.Fatal(err)
	}
	if cfg, err = loadConfig(*cfgPath); err != nil {
		log.Fatal(err)
	}
	if maps, err = loadMappings(orDefault(*transDir, "../translations")); err != nil {
		log.Fatal(err)
	}
	if *opsPath != "" {
		if err := loadRoutes(*opsPath); err != nil {
			log.Fatal(err)
		}
	}
	for _, p := range maps.pairs() {
		status := "ready"
		if !maps.byPair[p].usable() {
			status = "EMPTY - records that this generation has no source yet; requests for it will 501"
		}
		log.Printf("translation %s: %s", p, status)
	}
	if *cfgPath == "" {
		cfg.Default = profile{Offers: *offers, Delay: delay.String()}
	}

	orders = newOrderStore(*orderTTL, *orderMax)
	// A forwarded request waits out the owner's simulated latency too.
	peerClient.Timeout = *maxDelay + time.Minute
	prox = newProxyMode(*upstream, *recordTo, strings.Split(*fwdHdrs, ","))
	if prox != nil {
		mode := "proxy"
		if *recordTo != "" {
			mode = "proxy+record -> " + *recordTo
		}
		log.Printf("%s: %s", mode, *upstream)
	}

	loadTemplates()
	registerRoutes()

	log.Printf("listening on %s (default offers=%d delay=%s airline=%s)",
		*addr, cfg.Default.Offers, orDash(cfg.Default.Delay), orNone(*defAir))
	// Explicit timeouts: the zero-value server has none, which leaves a slow or
	// stalled client holding a connection indefinitely.
	srv := &http.Server{
		Addr:              *addr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// The write deadline runs from the end of the request headers, so it
		// covers the simulated latency as well as a large response on a slow link.
		WriteTimeout:   *maxDelay + 3*time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	log.Fatal(srv.ListenAndServe())
}

// loadTemplates reads one reference response per version, trip type and
// language. A version with no files present is skipped with a warning — the
// mock serves whatever it has.
func loadTemplates() {
	raws := map[string][]byte{} // native reference bytes, by template key
	load := func(key, path string, v *version) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		fixed, n := uniqueLegIDs(raw)
		if n > 0 {
			log.Printf("%s: renamed %d repeated DatedOperatingLegID (the schema requires them unique)", path, n)
		}
		t, err := newTemplate(fixed, v)
		if err != nil {
			log.Printf("warning: %s: %v", path, err)
			return
		}
		// Translation starts from the reference as captured: 21.3 lists each
		// operated leg once, so the repeats collapse there instead.
		templates[key], raws[key] = t, raw
		logTemplate(key, t)
	}

	for seg, v := range vers.Versions {
		dir := filepath.Join(*stubs, v.StubsDir)
		if _, err := os.Stat(dir); err != nil {
			// Flat layout: -stubs holds one version's files directly. Only the
			// version named by -flat-version may claim them, otherwise every
			// version would report ready while serving the same generation.
			if seg != *flatVer {
				continue
			}
			dir = *stubs
		}
		for _, lang := range []string{"default", "en", "es", "pt"} {
			load(seg+"|ow:"+lang, filepath.Join(dir, lang, "air-shopping-rs.xml"), v)
		}
		load(seg+"|rt", filepath.Join(dir, "air-shopping-rt-rs.xml"), v)
		// Multi-city references are keyed by itinerary shape, which only the
		// reference itself can tell.
		mcs, _ := filepath.Glob(filepath.Join(dir, "air-shopping-mc*.xml"))
		for _, f := range mcs {
			if raw, err := os.ReadFile(f); err == nil {
				if t, err := newTemplate(raw, v); err == nil && len(t.legs) > 0 {
					load(seg+"|mc:"+t.shape, f, v)
				} else if err != nil {
					log.Printf("warning: %s: %v", f, err)
				}
			}
		}
	}

	// A generation with no reference of its own is built by translating one
	// that has, once, here. Serving it is then no different from a native one.
	native := map[string]bool{}
	for key := range raws {
		seg, _, _ := strings.Cut(key, "|")
		native[seg] = true
	}
	for seg, v := range vers.Versions {
		if native[seg] {
			log.Printf("version %s (IATA %s) ready", seg, v.IATA)
			continue
		}
		from, m := "", (*mapping)(nil)
		for src := range native {
			if cand := maps.find(vers.Versions[src].IATA, v.IATA); cand.usable() {
				from, m = src, cand
				break
			}
		}
		if m == nil {
			log.Printf("version %s (IATA %s): no reference response and no usable mapping; requests will 501", seg, v.IATA)
			continue
		}
		built := 0
		for key, raw := range raws {
			if !strings.HasPrefix(key, from+"|") {
				continue
			}
			out, rep, err := m.translate(raw)
			if err == nil {
				var t *template
				if t, err = newTemplate(out, v); err == nil {
					t.xlate, t.rep = m, rep
					target := seg + strings.TrimPrefix(key, from)
					templates[target] = t
					logTemplate(target, t)
					built++
					continue
				}
			}
			log.Printf("warning: translating %s to %s: %v", key, v.IATA, err)
		}
		if built > 0 {
			log.Printf("version %s (IATA %s) ready, translated from %s", seg, v.IATA, vers.Versions[from].IATA)
		}
	}
	if len(raws) == 0 {
		log.Fatal("no reference response loaded for any version; nothing to serve")
	}
}

// mcShapes lists the multi-city itinerary shapes a version has references for.
func mcShapes(seg string) []string {
	var out []string
	for k := range templates {
		if shape, ok := strings.CutPrefix(k, seg+"|mc:"); ok {
			out = append(out, shape)
		}
	}
	sort.Strings(out)
	return out
}

func jsonLegs(legs []leg) []jsonLeg {
	out := make([]jsonLeg, 0, len(legs))
	for _, l := range legs {
		out = append(out, jsonLeg{l.From, l.To, l.Date})
	}
	return out
}

func logTemplate(key string, t *template) {
	trip := "OW"
	switch {
	case t.roundTrip:
		trip = "RT " + t.ref.retDate.Format(dateFmt)
	case len(t.legs) > 1:
		trip = "MC " + t.shape
	}
	how := ""
	if t.xlate != nil {
		how = fmt.Sprintf("  translated %s->%s, name coverage %d%%", t.xlate.From, t.xlate.To, t.rep.Coverage())
	}
	log.Printf("%-18s %s-%s %s %-14s carrier=%s cur=%s pax=%s  %d offers, %.1f MB%s",
		key, t.ref.origin, t.ref.dest, t.ref.depDate.Format(dateFmt), trip,
		t.ref.carrier, t.ref.currency, t.ref.pax, t.NaturalOffers(),
		float64(t.size)/(1<<20), how)
}

func registerRoutes() {
	http.HandleFunc(*tokenPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"ndc-mock-token","token_type":"Bearer","expires_in":3600}`)
	})
	http.HandleFunc("/__health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })

	for seg := range vers.Versions {
		base := "/ndc/" + seg + "/"
		for path, h := range routes {
			http.HandleFunc(base+path, withProxy(path, handlers[h]))
		}
	}
	http.HandleFunc("/__stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ordersHeld": orders.size(),
			"versions":   len(vers.Versions),
			"templates":  len(templates),
		})
	})
}

func airshopping(w http.ResponseWriter, r *http.Request) {
	seg := versionOf(r.URL.Path)
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		fault(w, http.StatusBadRequest, "could not read request body")
		return
	}
	q, err := parseQuery(body)
	if err != nil {
		fault(w, http.StatusBadRequest, err.Error())
		return
	}
	q.Lang = langOf(r)
	t := pick(seg, q)
	if t == nil && q.multiCity() {
		fault(w, http.StatusNotImplemented, fmt.Sprintf("%v: shape %s, loaded %s", errMultiCity,
			pattern(q.Legs), orDefault(strings.Join(mcShapes(seg), " "), "none")))
		return
	}
	if t == nil {
		fault(w, http.StatusNotImplemented, "no reference response for version "+seg+
			" and no translation mapping to reach it")
		return
	}

	air := airs.get(firstNonEmpty(r.Header.Get("X-Mock-Airline"), cfg.airlineFor(q.Origin, q.Dest), *defAir))
	n, d := cfg.resolveBounded(q.Origin, q.Dest, r.Header.Get("X-Mock-Offers"), r.Header.Get("X-Mock-Delay"), *maxOffers, *maxDelay)
	if d > 0 {
		time.Sleep(d)
	}

	// Echo headers are set once, before the format branch: they describe what was
	// served and must agree whichever representation goes out.
	w.Header().Set("X-Mock-Offers", strconv.Itoa(orNatural(n, t)))
	w.Header().Set("X-Mock-Version", seg)
	if t.xlate != nil {
		w.Header().Set("X-Mock-Translated", t.xlate.From+"->"+t.xlate.To)
		w.Header().Set("X-Mock-Translation-Coverage", fmt.Sprintf("%d%%", t.rep.Coverage()))
		if t.rep.Unverified > 0 {
			w.Header().Set("X-Mock-Translation-Unverified", strconv.Itoa(t.rep.Unverified))
		}
	}
	if air != nil {
		w.Header().Set("X-Mock-Airline", air.Code)
	}

	// The JSON projection is built from the decoded identifiers rather than by
	// converting the document: a BFF returns far less than NDC does, and
	// parsing multiple megabytes per request would dominate the response time.
	if formatOf(r) == formatJSON {
		count := orNatural(n, t)
		offs := make([]jsonOffer, 0, count)
		for k := 0; k < count; k++ {
			// The real identifier travels in the projection: it is what the
			// caller hands back to price and order, and it is self-describing.
			// Substituting a synthetic label here would break the chain and the
			// next call would silently fall back to defaults.
			id := t.offers[k%len(t.offers)].encode()
			info := describe(id)
			info.setLegs(q.Legs)
			info.ADT, info.CHD, info.INF = q.ADT, q.CHD, q.INF
			if air != nil {
				info.Carrier = air.Code
				info.Currency, info.Amount = air.convert(info.Currency, info.Amount)
			}
			offs = append(offs, offerToJSON(reencode(id, info), info))
		}
		writeJSON(w, http.StatusOK, jsonShopping{
			ShoppingResponseID: "SHOP-" + q.Origin + q.Dest + "-" + q.DepDate,
			Origin:             q.Origin, Destination: q.Dest,
			DepartureDate: q.DepDate, ReturnDate: q.RetDate, Legs: jsonLegs(q.Legs),
			Currency:   firstNonEmpty(offerCurrency(offs), t.ref.currency),
			OfferCount: count, Offers: offs,
		})
		return
	}

	bp := bufs.Get().(*[]byte)
	b := t.render((*bp)[:0], q, n, air)
	w.Header().Set("Content-Type", "application/xml;charset=UTF-8")
	w.Write(b)
	// Only recycle buffers of a sane size: a large response would otherwise pin
	// its capacity in the pool for the life of the process.
	if cap(b) <= 32<<20 {
		*bp = b
		bufs.Put(bp)
	}
}

// langOf reads the response language from the configured header, falling back
// to Accept-Language. Only the primary subtag is used ("es-CL" -> "es").
func langOf(r *http.Request) string {
	v := r.Header.Get(*langHdr)
	if v == "" {
		v = r.Header.Get("Accept-Language")
	}
	v, _, _ = strings.Cut(v, ",")
	v, _, _ = strings.Cut(v, "-")
	return strings.ToLower(strings.TrimSpace(v))
}

// withProxy sends the request upstream when proxy mode is on, falling back to
// the synthetic handler if the provider is unreachable, so an upstream outage
// degrades a load test rather than ending it.
func withProxy(op string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if prox == nil {
			h(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			failure(w, r, http.StatusBadRequest, "could not read request body")
			return
		}
		if prox.handle(w, r, body, op) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		w.Header().Set("X-Mock-Mode", "synthetic-fallback")
		h(w, r)
	}
}

// versionOf pulls the version segment out of /ndc/<version>/<operation>.
func versionOf(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// pick selects the reference: round trip when the request carries a return leg,
// otherwise the one matching the requested language.
func pick(seg string, q query) *template {
	if q.multiCity() {
		return templates[seg+"|mc:"+pattern(q.Legs)]
	}
	if q.RetDate != "" {
		if t := templates[seg+"|rt"]; t != nil {
			return t
		}
	}
	if t := templates[seg+"|ow:"+q.Lang]; t != nil {
		return t
	}
	return templates[seg+"|ow:default"]
}

func fault(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/xml;charset=UTF-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+
		`<IATA_AirShoppingRS xmlns="http://www.iata.org/IATA/2015/00/2019.2/IATA_AirShoppingRS">`+
		`<Error><Code>%d</Code><DescText>ndc-mock: %s</DescText><OwnerName>NDC_MOCK</OwnerName></Error>`+
		`</IATA_AirShoppingRS>`, code, escape(msg))
}

func orDash(s string) string {
	if s == "" || s == "0s" {
		return "0s (uncalibrated)"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(reference)"
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func offerCurrency(o []jsonOffer) string {
	if len(o) == 0 {
		return ""
	}
	return o[0].Price.Currency
}

func orNatural(n int, t *template) int {
	if n <= 0 {
		return t.NaturalOffers()
	}
	return n
}
