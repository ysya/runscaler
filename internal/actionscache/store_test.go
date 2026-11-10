package actionscache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func saveEntry(t *testing.T, store *Store, p Principal, key string, payload string) Entry {
	t.Helper()
	ctx := context.Background()
	reservation, err := store.Reserve(ctx, p, key, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutBlob(ctx, reservation.ID, bytes.NewReader([]byte(payload))); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Finalize(ctx, p, key, "v1", int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestStoreSweeperPinsReadersAndRemovesStaleReservations(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := Principal{RepositoryID: "repo", Grants: []Grant{{Scope: "main", Permission: 3}}}
	first := saveEntry(t, store, p, "first", "12345678")
	second := saveEntry(t, store, p, "second", "abcdefgh")
	reader, err := store.OpenEntry(first)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := store.Sweep(context.Background(), 7, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Lookup(context.Background(), p, "first", nil, "v1"); err != nil || !ok {
		t.Fatalf("pinned reader evicted: %v, %v", ok, err)
	}
	if _, ok, err := store.Lookup(context.Background(), p, "second", nil, "v1"); err != nil || ok {
		t.Fatalf("unpinned entry retained: %v, %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "objects", second.ID)); !os.IsNotExist(err) {
		t.Fatalf("evicted object remains: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Sweep(context.Background(), 7, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Lookup(context.Background(), p, "first", nil, "v1"); err != nil || ok {
		t.Fatalf("unpinned old entry retained: %v, %v", ok, err)
	}
	reservation, err := store.Reserve(context.Background(), p, "stale", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE entries SET expires_at=1 WHERE object_id=?`, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Sweep(context.Background(), 1024, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(context.Background(), p, "stale", "v1"); err != nil {
		t.Fatalf("stale reservation still conflicts: %v", err)
	}
}

func TestStorePurgesDanglingEntry(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := Principal{RepositoryID: "repo", Grants: []Grant{{Scope: "main", Permission: 3}}}
	entry := saveEntry(t, store, p, "dangling", "complete")
	if err := os.Remove(store.partPath(entry.ID, "__single__")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Lookup(context.Background(), p, "dangling", nil, "v1"); err != nil || ok {
		t.Fatalf("dangling hit exposed: %v, %v", ok, err)
	}
	if _, err := store.Reserve(context.Background(), p, "dangling", "v1"); err != nil {
		t.Fatalf("dangling identity not purged: %v", err)
	}
}

func TestStorePublishesOnlyCompleteImmutableEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "native")
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := Principal{RepositoryID: "repo-a", Grants: []Grant{{Scope: "refs/heads/main", Permission: 3}}}
	ctx := context.Background()
	reservation, err := store.Reserve(ctx, p, "archive", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(ctx, p, "archive", "v1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent reservation = %v", err)
	}
	if entry, ok, err := store.Lookup(ctx, p, "archive", nil, "v1"); err != nil || ok {
		t.Fatalf("partial entry visible: %+v, %v, %v", entry, ok, err)
	}
	if err := store.PutPart(ctx, reservation.ID, "second", bytes.NewReader([]byte("world"))); err != nil {
		t.Fatal(err)
	}
	if err := store.PutPart(ctx, reservation.ID, "first", bytes.NewReader([]byte("hello "))); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBlockList(ctx, reservation.ID, []string{"first", "second"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(ctx, p, "archive", "v1", 10); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("wrong size accepted: %v", err)
	}
	entry, err := store.Finalize(ctx, p, "archive", "v1", 11)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != 11 {
		t.Fatalf("size = %d", entry.Size)
	}
	if _, err := store.Reserve(ctx, p, "archive", "v1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed key replaced: %v", err)
	}
	reader, err := store.OpenEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(content) != "hello world" {
		t.Fatalf("ordered content = %q, %v", content, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Lookup(ctx, p, "archive", nil, "v1")
	if err != nil || !ok || got.ID != entry.ID {
		t.Fatalf("restart lookup = %+v, %v, %v", got, ok, err)
	}
}

func TestStoreScopesPrefixAndConcurrentWriters(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	writer := Principal{RepositoryID: "repo-a", Grants: []Grant{{Scope: "refs/heads/main", Permission: 3}}}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, err := store.Reserve(ctx, writer, "same", "v1")
			if err == nil {
				err = store.PutPart(ctx, reservation.ID, "blob", bytes.NewReader([]byte("complete")))
			}
			if err == nil {
				err = store.SetBlockList(ctx, reservation.ID, []string{"blob"})
			}
			if err == nil {
				_, err = store.Finalize(ctx, writer, "same", "v1", 8)
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrConflict):
			conflict++
		default:
			t.Fatalf("writer error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	reader := Principal{RepositoryID: "repo-a", Grants: []Grant{{Scope: "refs/heads/main", Permission: 1}}}
	if entry, ok, err := store.Lookup(ctx, reader, "missing", []string{"sa"}, "v1"); err != nil || !ok || entry.Key != "same" {
		t.Fatalf("prefix lookup = %+v, %v, %v", entry, ok, err)
	}
	otherRepo := Principal{RepositoryID: "repo-b", Grants: reader.Grants}
	if _, ok, err := store.Lookup(ctx, otherRepo, "same", nil, "v1"); err != nil || ok {
		t.Fatalf("repository isolation failed: %v, %v", ok, err)
	}
	otherBranch := Principal{RepositoryID: "repo-a", Grants: []Grant{{Scope: "refs/heads/other", Permission: 1}}}
	if _, ok, err := store.Lookup(ctx, otherBranch, "same", nil, "v1"); err != nil || ok {
		t.Fatalf("branch isolation failed: %v, %v", ok, err)
	}
}
