package main

import (
	"strings"
	"testing"
)

func TestUniqueLegIDs(t *testing.T) {
	in := `<a><DatedOperatingLegID>L1</DatedOperatingLegID><DatedOperatingLegID>L2</DatedOperatingLegID><DatedOperatingLegID>L1</DatedOperatingLegID><DatedOperatingLegID>L1</DatedOperatingLegID></a>`
	out, n := uniqueLegIDs([]byte(in))
	if n != 2 || !strings.Contains(string(out), ">L1_2<") || !strings.Contains(string(out), ">L1_3<") || strings.Count(string(out), ">L1<") != 1 {
		t.Errorf("renamed %d: %s", n, out)
	}
}
