package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// version describes one IATA NDC generation: which URL segment serves it and
// which element names the slicer needs. The engine works on text markers rather
// than a schema, so supporting a new generation is a matter of declaring its
// element names and supplying a reference response — no code change.
type version struct {
	IATA        string   `json:"iata"`
	Namespace   string   `json:"namespace"`
	OfferElem   string   `json:"offerElement"`
	IDElems     []string `json:"idElements"`
	StubsDir    string   `json:"stubs"`
	PathSegment string   `json:"-"` // key in the map, e.g. "v192"
}

type versionSet struct {
	Versions map[string]*version `json:"versions"`
}

func loadVersions(path string) (*versionSet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vs := &versionSet{}
	if err := json.Unmarshal(b, vs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(vs.Versions) == 0 {
		return nil, fmt.Errorf("%s: no versions declared", path)
	}
	for seg, v := range vs.Versions {
		v.PathSegment = seg
		if v.OfferElem == "" {
			v.OfferElem = "Offer"
		}
		if len(v.IDElems) == 0 {
			return nil, fmt.Errorf("%s: version %s declares no idElements", path, seg)
		}
	}
	return vs, nil
}
