package main

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// profile is what a route is worth simulating as: how many offers to return,
// how long to take, and which carrier to present it as.
type profile struct {
	Offers  int    `json:"offers"`  // 0 = as many as the reference response holds
	Delay   string `json:"delay"`   // "850ms" fixed, "850ms~2.4s" p50~p95; empty = none
	Airline string `json:"airline"` // carrier code; empty = keep the reference carrier
}

type config struct {
	Default profile            `json:"default"`
	Routes  map[string]profile `json:"routes"`
	// Operations holds the latency of everything after AirShopping, keyed by
	// path ("order/cancel") or by handler ("ordercancel"). Not per route: those
	// calls carry an offer, not a search, and their cost does not track it.
	Operations map[string]string `json:"operations"`
}

func loadConfig(path string) (*config, error) {
	c := &config{Routes: map[string]profile{}}
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Routes == nil {
		c.Routes = map[string]profile{}
	}
	for k, p := range c.Routes {
		if _, err := parseDelay(p.Delay); err != nil {
			return nil, fmt.Errorf("%s: route %s: %w", path, k, err)
		}
	}
	if _, err := parseDelay(c.Default.Delay); err != nil {
		return nil, fmt.Errorf("%s: default: %w", path, err)
	}
	for op, d := range c.Operations {
		if _, err := parseDelay(d); err != nil {
			return nil, fmt.Errorf("%s: operation %s: %w", path, op, err)
		}
	}
	return c, nil
}

// latency is a delay to simulate: fixed when p95 equals p50, otherwise drawn
// from a log-normal through both points. A fixed delay at the median
// understates what a client holds open, because real latency has a tail and
// in-flight requests scale with the mean, not the median.
type latency struct{ p50, p95 time.Duration }

func parseDelay(s string) (latency, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return latency{}, nil
	}
	lo, hi, ranged := strings.Cut(s, "~")
	p50, err := time.ParseDuration(strings.TrimSpace(lo))
	if err != nil {
		return latency{}, err
	}
	if !ranged {
		return latency{p50, p50}, nil
	}
	p95, err := time.ParseDuration(strings.TrimSpace(hi))
	if err != nil {
		return latency{}, err
	}
	if p50 <= 0 || p95 < p50 {
		return latency{}, fmt.Errorf("%q: want p50~p95 with 0 < p50 <= p95", s)
	}
	return latency{p50, p95}, nil
}

// z95 is the standard normal quantile at 0.95.
const z95 = 1.6448536269514722

func (l latency) sample() time.Duration {
	if l.p95 <= l.p50 {
		return l.p50
	}
	sigma := math.Log(float64(l.p95)/float64(l.p50)) / z95
	return time.Duration(float64(l.p50) * math.Exp(sigma*rand.NormFloat64()))
}

// clampOffers bounds a requested offer count. Without a ceiling a single
// header multiplies into gigabytes: each offer is tens of kilobytes and the
// whole document is assembled in memory before it is written.
func clampOffers(n, max int) int {
	if max > 0 && n > max {
		return max
	}
	return n
}

// clampDelay bounds simulated latency so a caller cannot hold connections open.
func clampDelay(d, max time.Duration) time.Duration {
	if max > 0 && d > max {
		return max
	}
	return d
}

// resolve settles offer count and latency. Precedence: X-Mock-* headers (so a
// load test can sweep values without editing files) > route profile > default.
func (c *config) resolve(origin, dest, hdrOffers, hdrDelay string) (int, time.Duration) {
	p := c.merged(origin, dest)
	offers := p.Offers
	if n, err := strconv.Atoi(hdrOffers); err == nil && n > 0 {
		offers = n
	}
	delay, _ := parseDelay(p.Delay)
	if d, err := parseDelay(hdrDelay); err == nil && d.p50 > 0 {
		delay = d
	}
	return offers, delay.sample()
}

// resolveBounded is resolve with the operator's ceilings applied.
func (c *config) resolveBounded(origin, dest, hdrOffers, hdrDelay string, maxN int, maxD time.Duration) (int, time.Duration) {
	n, d := c.resolve(origin, dest, hdrOffers, hdrDelay)
	return clampOffers(n, maxN), clampDelay(d, maxD)
}

// opDelay samples the configured latency for a post-shopping operation, keyed
// by its path or, failing that, by the handler the path routes to.
func (c *config) opDelay(op string) time.Duration {
	v, ok := c.Operations[op]
	if !ok {
		v = c.Operations[routes[op]]
	}
	d, _ := parseDelay(v)
	return d.sample()
}

// airlineFor returns the carrier configured for a route, if any.
func (c *config) airlineFor(origin, dest string) string {
	return c.merged(origin, dest).Airline
}

func (c *config) merged(origin, dest string) profile {
	p := c.Default
	if r, ok := c.Routes[origin+"-"+dest]; ok {
		if r.Offers != 0 {
			p.Offers = r.Offers
		}
		if r.Delay != "" {
			p.Delay = r.Delay
		}
		if r.Airline != "" {
			p.Airline = r.Airline
		}
	}
	return p
}
