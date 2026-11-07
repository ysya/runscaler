package diagnostics

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorePrivateBoundedRedactedAndRetained(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(parent, "diagnostics")
	s := NewStore(dir, true)
	for i := 0; i < 23; i++ {
		if _, err := s.Save(context.Background(), Record{Reason: "readiness_timeout", RawLog: "Authorization: Bearer private\nknown-token\n-----BEGIN PRIVATE KEY-----\nprivate\n-----END PRIVATE KEY-----\n" + strings.Repeat("line\n", 150)}, "known-token"); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 20 {
		t.Fatalf("files=%d", len(entries))
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal("directory is not private")
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "known-token") || strings.Contains(string(data), "private") || strings.Count(string(data), "line\\n") > 100 {
		t.Fatalf("unsafe report: %s", data)
	}
	info, _ = os.Stat(filepath.Join(dir, entries[0].Name()))
	if info.Mode().Perm() != 0600 {
		t.Fatal("file is not private")
	}
	unrelated := filepath.Join(dir, "operator-notes.json")
	_ = os.WriteFile(unrelated, []byte("keep"), 0600)
	old := filepath.Join(dir, entries[0].Name())
	past := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(old, past, past)
	if _, err := s.Save(context.Background(), Record{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired report retained")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("unowned file removed")
	}
}
func TestStoreRejectsSymlinkAndDefaultsToNoLogs(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	target := filepath.Join(parent, "target")
	_ = os.Mkdir(target, 0700)
	link := filepath.Join(parent, "link")
	_ = os.Symlink(target, link)
	if _, err := NewStore(link, false).Save(context.Background(), Record{}); err == nil {
		t.Fatal("followed symlink")
	}
	path, err := NewStore(filepath.Join(parent, "safe"), false).Save(context.Background(), Record{RawLog: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "secret") {
		t.Fatal("logs captured by default")
	}
}

func TestStoreRedactionAndTotalByteBudget(t *testing.T) {
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	s := NewStore(filepath.Join(parent, "reports"), true)
	path, err := s.Save(t.Context(), Record{RawLog: "Authorization: Bearer unknown\nknown-jit-material\n-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----\nknown-pat-prefix"}, "known-jit-material", "known-pat-prefix-rest")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, secret := range []string{"unknown", "known-jit", "known-pat", "PRIVATE KEY", "\\nkey\\n"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("secret %q in report: %s", secret, data)
		}
	}
	for i := 0; i < 20; i++ {
		if _, err := s.Save(t.Context(), Record{RawLog: strings.Repeat("\x00", 64*1024)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	var size int64
	for _, e := range entries {
		info, _ := e.Info()
		size += info.Size()
	}
	if size > 2*1024*1024 {
		t.Fatalf("report bytes=%d", size)
	}
}
