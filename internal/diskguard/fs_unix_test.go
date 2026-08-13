//go:build darwin || linux

package diskguard

import "testing"

func TestStatForGroupsSamePathsTogether(t *testing.T) {
	dir := t.TempDir()
	a, err := StatFor(dir)
	if err != nil {
		t.Fatalf("StatFor: %v", err)
	}
	b, err := StatFor(dir + "/.")
	if err != nil {
		t.Fatalf("StatFor: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("same filesystem must share an ID, got %q and %q", a.ID, b.ID)
	}
	if a.TotalBytes == 0 {
		t.Error("TotalBytes should be non-zero for a real filesystem")
	}
}
