package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A new process starts numbering again; it must not overwrite what an earlier
// run recorded.
func TestRecordNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	rq := []byte(`<OriginDepCriteria><IATA_LocationCode>GRU</IATA_LocationCode></OriginDepCriteria><DestArrivalCriteria><IATA_LocationCode>NAT</IATA_LocationCode></DestArrivalCriteria>`)
	(&proxyMode{recordTo: dir}).record("airshopping", rq, []byte("first"))
	(&proxyMode{recordTo: dir}).record("airshopping", rq, []byte("second"))
	got, _ := filepath.Glob(filepath.Join(dir, "*-rs.xml"))
	if len(got) != 2 {
		t.Fatalf("want 2 captures, got %v", got)
	}
	if b, _ := os.ReadFile(got[0]); string(b) != "first" {
		t.Errorf("the first capture was overwritten: %q", b)
	}
}
