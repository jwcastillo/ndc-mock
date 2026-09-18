package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
)

// airline is the profile used to make a synthetic response plausible for a
// given carrier: its code, selling currency, hubs and a fare multiplier.
//
// Fares keep the *distribution* of the reference response — real pricing
// produces a spread across brands and cabins that is hard to fake — and only
// the currency and magnitude are adjusted.
type airline struct {
	Code        string   `json:"-"`
	Name        string   `json:"name"`
	Currency    string   `json:"currency"`
	Hubs        []string `json:"hubs"`
	FareScale   float64  `json:"fareScale"`
	FlightRange []int    `json:"flightRange"`

	fx map[string]float64 // shared rate table, injected on load
}

type airlineSet struct {
	Airlines map[string]*airline `json:"airlines"`
	FxToUSD  map[string]float64  `json:"fxToUSD"`
}

func loadAirlines(path string) (*airlineSet, error) {
	if path == "" {
		return &airlineSet{Airlines: map[string]*airline{}}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	as := &airlineSet{}
	if err := json.Unmarshal(b, as); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for code, a := range as.Airlines {
		a.Code = code
		if a.FareScale == 0 {
			a.FareScale = 1
		}
		a.fx = as.FxToUSD
	}
	return as, nil
}

func (as *airlineSet) get(code string) *airline {
	if as == nil || code == "" {
		return nil
	}
	return as.Airlines[strings.ToUpper(code)]
}

// convert moves a fare from the reference currency into this carrier's,
// preserving economic value and applying the carrier's fare scale.
func (a *airline) convert(cur string, amount int) (string, int) {
	if a.Currency == "" || a.fx == nil {
		return cur, amount
	}
	from, ok := a.fx[cur]
	if !ok {
		from = 1
	}
	to, ok := a.fx[a.Currency]
	if !ok || to == 0 {
		return cur, amount
	}
	usd := float64(amount) * from * a.FareScale
	return a.Currency, int(math.Round(usd / to))
}

// serves reports whether the route plausibly belongs to this carrier's network,
// i.e. one end touches a hub. Used only to warn: a load test may well want to
// drive traffic the real network would not carry.
func (a *airline) serves(origin, dest string) bool {
	if len(a.Hubs) == 0 {
		return true
	}
	for _, h := range a.Hubs {
		if h == origin || h == dest {
			return true
		}
	}
	return false
}
