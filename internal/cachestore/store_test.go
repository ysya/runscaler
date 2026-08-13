package cachestore

import (
	"slices"
	"testing"
)

func TestTiersFor(t *testing.T) {
	tests := []struct {
		kind StoreKind
		want []Tier
	}{
		{KindGarbage, []Tier{Tier1}},
		{KindCache, []Tier{Tier2, Tier4}},
		{KindScratch, []Tier{Tier3}},
	}
	for _, tt := range tests {
		got := TiersFor(tt.kind)
		if !slices.Equal(got, tt.want) {
			t.Errorf("TiersFor(%v) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

// TestScratchNeverReclaimedWholesale pins the invariant that protects
// in-flight workflow handoff data: a scratch store is only ever reclaimable
// at Tier3 (its TTL-expired portion), never at the wholesale Tier4.
func TestScratchNeverReclaimedWholesale(t *testing.T) {
	for _, tier := range TiersFor(KindScratch) {
		if tier == Tier4 {
			t.Fatal("KindScratch must never be reclaimable at Tier4 — " +
				"wholesale removal would destroy an in-flight run's handoff data")
		}
	}
}
