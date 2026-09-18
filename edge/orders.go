package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// order is the state the ordering half of the flow needs. Shopping and pricing
// are stateless; OrderCreate onwards is not, so the mock keeps a small
// in-memory store. Entries expire so a long load test cannot grow unbounded.
type order struct {
	ID        string
	Status    string // Opened, Cancelled, Changed
	Info      offerInfo
	Pax       []passenger
	Services  []service
	Seats     []seat
	CreatedAt time.Time
	History   []string
}

type passenger struct {
	ID        string
	PTC       string
	Given     string
	Surname   string
	Birthdate string
}

type service struct {
	ID       string
	Code     string
	Name     string
	PaxID    string
	Currency string
	Amount   int
}

type seat struct {
	PaxID    string
	Row      int
	Column   string
	Currency string
	Amount   int
}

type orderStore struct {
	mu     sync.RWMutex
	byID   map[string]*order
	ttl    time.Duration
	maxLen int
}

func newOrderStore(ttl time.Duration, max int) *orderStore {
	s := &orderStore{byID: map[string]*order{}, ttl: ttl, maxLen: max}
	go s.reap()
	return s
}

func (s *orderStore) put(o *order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Under load the store is a cache, not a database: once it is full the
	// oldest entry goes, because a load test never revisits it.
	if len(s.byID) >= s.maxLen {
		var oldestID string
		var oldest time.Time
		for id, e := range s.byID {
			if oldestID == "" || e.CreatedAt.Before(oldest) {
				oldestID, oldest = id, e.CreatedAt
			}
		}
		delete(s.byID, oldestID)
	}
	s.byID[o.ID] = o
}

// get returns a copy, so a response can be rendered while another request
// changes the order.
func (s *orderStore) get(id string) (order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.byID[id]
	if !ok {
		return order{}, false
	}
	return o.clone(), true
}

// mutate applies fn under the lock and returns the result as a copy.
func (s *orderStore) mutate(id string, fn func(*order)) (order, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.byID[id]
	if !ok {
		return order{}, false
	}
	fn(o)
	return o.clone(), true
}

func (o *order) clone() order {
	c := *o
	c.Pax, c.Services, c.Seats = slices.Clone(o.Pax), slices.Clone(o.Services), slices.Clone(o.Seats)
	c.History = slices.Clone(o.History)
	return c
}

func (s *orderStore) reap() {
	for range time.Tick(time.Minute) {
		cut := time.Now().Add(-s.ttl)
		s.mu.Lock()
		for id, o := range s.byID {
			if o.CreatedAt.Before(cut) {
				delete(s.byID, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *orderStore) size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byID)
}

// newOrderID builds a record-locator-shaped identifier: a carrier prefix, a
// numeric block and a short alphanumeric tail, which is the usual shape.
func newOrderID(carrier string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	n := binary.BigEndian.Uint32(b[0:4]) % 10000000
	const alpha = "ABCDEFGHIJKLMNPQRSTUVWXYZ23456789"
	tail := make([]byte, 4)
	for i := range tail {
		tail[i] = alpha[int(b[4+i])%len(alpha)]
	}
	id := carrier + pad7(n) + string(tail)
	if *advertise != "" {
		id += "." + base64.RawURLEncoding.EncodeToString([]byte(*advertise))
	}
	return id
}

// --- several replicas -------------------------------------------------------
//
// Orders live in the memory of the replica that created them. Behind a load
// balancer the next call for that order usually lands elsewhere, so with
// -advertise set each order identifier carries its owner's address and a
// replica that does not hold the order forwards the request to the one that
// does. State stays in one place; no shared store is needed.

// ownerOf returns the address an order identifier names as its owner.
func ownerOf(id string) string {
	i := strings.LastIndexByte(id, '.')
	if i < 0 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(id[i+1:])
	if err != nil {
		return ""
	}
	return string(b)
}

// isPeer accepts an owner only if it resolves inside the -peers name. The
// identifier arrives from the client; without this check a crafted one turns
// the mock into a proxy to any address it can reach.
func isPeer(owner string) bool {
	host, _, err := net.SplitHostPort(owner)
	if err != nil || *peers == "" {
		return false
	}
	ips, err := net.LookupHost(*peers)
	if err != nil {
		return false
	}
	return slices.Contains(ips, host)
}

var peerClient = &http.Client{} // timeout set from -max-delay at startup

// forward replays the request against the owning replica and relays its answer.
// The marker header stops a request from bouncing between replicas.
func forward(w http.ResponseWriter, r *http.Request, owner string, body []byte) {
	rq, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+owner+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		failure(w, r, http.StatusBadGateway, "cannot forward to owner "+owner)
		return
	}
	rq.Header = r.Header.Clone()
	rq.Header.Set("X-Mock-Forwarded", *advertise)
	rs, err := peerClient.Do(rq)
	if err != nil {
		failure(w, r, http.StatusBadGateway, "owner "+owner+" unreachable (a restarted replica loses its orders)")
		return
	}
	defer rs.Body.Close()
	for k, v := range rs.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(rs.StatusCode)
	io.Copy(w, rs.Body)
}

func pad7(n uint32) string {
	s := strconv.FormatUint(uint64(n), 10)
	if len(s) >= 7 {
		return s
	}
	return strings.Repeat("0", 7-len(s)) + s
}

// buildPax materialises the passenger list an order carries, with synthetic
// names. Load tests care about payload shape, not about who is flying.
func buildPax(info offerInfo) []passenger {
	ids := info.paxIDs()
	out := make([]passenger, 0, len(ids))
	for n, id := range ids {
		out = append(out, passenger{
			ID:        id,
			PTC:       ptcOf(id),
			Given:     "TEST",
			Surname:   "PASSENGER" + strconv.Itoa(n+1),
			Birthdate: birthdateFor(ptcOf(id)),
		})
	}
	return out
}

func birthdateFor(ptc string) string {
	switch ptc {
	case "CHD":
		return "2018-05-10"
	case "INF":
		return "2025-03-02"
	default:
		return "1990-01-15"
	}
}
