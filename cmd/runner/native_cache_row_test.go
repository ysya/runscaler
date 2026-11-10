package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/ysya/runscaler/internal/actionscache"
	"github.com/ysya/runscaler/internal/config"
)

func TestNativeActionsCacheRowReportsCompletedBytes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	store, err := actionscache.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := actionscache.Principal{RepositoryID: "repo", Grants: []actionscache.Grant{{Scope: "main", Permission: 3}}}
	reservation, err := store.Reserve(context.Background(), p, "key", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutBlob(context.Background(), reservation.ID, bytes.NewReader([]byte("12345"))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finalize(context.Background(), p, "key", "v1", 5); err != nil {
		t.Fatal(err)
	}
	row := nativeActionsCacheRow(config.ActionsCacheConfig{Enabled: true, DataDir: root, MaxSizeBytes: 1024})
	if row.Store != "actions-cache-v2" || row.SizeBytes != 5 || row.Entries != 1 || row.MeasureError != "" {
		t.Fatalf("row = %+v", row)
	}
}
