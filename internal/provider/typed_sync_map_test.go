package provider

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestTypedSyncMapZeroValueAndReplacement(t *testing.T) {
	var m typedSyncMap[string, *warmVM]
	if got, ok := m.Load("vm"); ok || got != nil {
		t.Fatalf("missing entry = %v, %v", got, ok)
	}
	if got, ok := m.LoadAndDelete("vm"); ok || got != nil {
		t.Fatalf("missing removal = %v, %v", got, ok)
	}
	first, second := &warmVM{name: "first"}, &warmVM{name: "second"}
	m.Store("vm", first)
	if got, ok := m.Load("vm"); !ok || got != first {
		t.Fatalf("stored entry = %v, %v", got, ok)
	}
	m.Store("vm", second)
	if got, ok := m.LoadAndDelete("vm"); !ok || got != second {
		t.Fatalf("replacement removal = %v, %v", got, ok)
	}
	if _, ok := m.Load("vm"); ok {
		t.Fatal("removed entry remains present")
	}
}

func TestTypedSyncMapPreservesPresentNil(t *testing.T) {
	var m typedSyncMap[int, any]
	m.Store(1, nil)
	if got, ok := m.Load(1); !ok || got != nil {
		t.Fatalf("present nil = %v, %v", got, ok)
	}
	if got, ok := m.LoadAndDelete(1); !ok || got != nil {
		t.Fatalf("present nil removal = %v, %v", got, ok)
	}
	if _, ok := m.Load(1); ok {
		t.Fatal("nil entry remains present after removal")
	}
}

func TestTypedSyncMapConcurrentLoadAndDeleteIsAtomic(t *testing.T) {
	var m typedSyncMap[string, *warmVM]
	vm := &warmVM{name: "shared"}
	m.Store("vm", vm)
	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		wg.Go(func() {
			<-start
			if got, ok := m.LoadAndDelete("vm"); ok {
				if got != vm {
					t.Errorf("removed %v, want shared VM", got)
				}
				winners.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("removal winners = %d, want 1", got)
	}
	if _, ok := m.Load("vm"); ok {
		t.Fatal("removed entry remains present")
	}
}
