package main

import (
	"context"
	"slices"
	"testing"
)

func TestCheckTartVMsAppliesStartupOwnershipRules(t *testing.T) {
	mc := &mockCommandRunner{}
	mc.setResult("tart list --format json", `[
	  {"Name":"runner-deadbeef","Source":"local","State":"running"},
	  {"Name":"pool-0-1778066579017","Source":"local","State":"stopped"},
	  {"Name":"runner-keep-0a1b2c3d","Source":"local","State":"stopped"},
	  {"Name":"runner-nothex00","Source":"local","State":"running"},
	  {"Name":"pool-x-1778066579017","Source":"local","State":"running"},
	  {"Name":"runner-keep-golden","Source":"local","State":"stopped"},
	  {"Name":"runner-0011aabb","Source":"OCI","State":"stopped"}
	]`)
	remaining, err := checkTartVMs(context.Background(), mc, "/Users/ci/.tart", false)
	if err != nil || remaining != 3 {
		t.Fatalf("report = %d, %v; want the 3 runner-owned VMs", remaining, err)
	}
	if len(mc.calls) != 1 {
		t.Fatalf("report mode ran %v, want only the listing", mc.calls)
	}
	remaining, err = checkTartVMs(context.Background(), mc, "/Users/ci/.tart", true)
	if err != nil || remaining != 0 {
		t.Fatalf("fix = %d, %v", remaining, err)
	}
	for _, forbidden := range []string{"tart delete runner-nothex00", "tart delete pool-x-1778066579017", "tart delete runner-keep-golden", "tart delete runner-0011aabb"} {
		if slices.Contains(mc.calls, forbidden) {
			t.Errorf("doctor deleted a VM runner did not create: %s", forbidden)
		}
	}
	for _, want := range []string{"tart delete runner-deadbeef", "tart delete pool-0-1778066579017", "tart delete runner-keep-0a1b2c3d"} {
		if !slices.Contains(mc.calls, want) {
			t.Errorf("missing %s in %v", want, mc.calls)
		}
	}
}
