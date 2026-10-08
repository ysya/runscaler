package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/config"
)

// fakeScaleSetAPI holds at most one scale set per runner group and records
// what was asked of it.
type fakeScaleSetAPI struct {
	groups   map[string]int                   // runner group name -> ID
	existing map[int]*scaleset.RunnerScaleSet // runner group ID -> its scale set
	created  []*scaleset.RunnerScaleSet
	updated  []int
	deleted  []int
	lookedUp []int // runner group IDs passed to GetRunnerScaleSet
}

func (f *fakeScaleSetAPI) GetRunnerGroupByName(_ context.Context, name string) (*scaleset.RunnerGroup, error) {
	return &scaleset.RunnerGroup{ID: f.groups[name], Name: name}, nil
}

func (f *fakeScaleSetAPI) GetRunnerScaleSet(_ context.Context, groupID int, _ string) (*scaleset.RunnerScaleSet, error) {
	f.lookedUp = append(f.lookedUp, groupID)
	return f.existing[groupID], nil
}

func (f *fakeScaleSetAPI) CreateRunnerScaleSet(_ context.Context, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.created = append(f.created, rs)
	created := *rs
	created.ID = 900
	return &created, nil
}

func (f *fakeScaleSetAPI) UpdateRunnerScaleSet(_ context.Context, id int, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.updated = append(f.updated, id)
	updated := *rs
	updated.ID = id
	return &updated, nil
}

func (f *fakeScaleSetAPI) DeleteRunnerScaleSet(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}

func iosScaleSet() config.ScaleSetConfig {
	return config.ScaleSetConfig{
		ScaleSetName: "FrankMacStudioMacOS",
		Labels:       []string{"self-hosted", "macOS", "ios-builder-scaler"},
	}
}

func TestEnsureScaleSet_CreatesWhenMissing(t *testing.T) {
	api := &fakeScaleSetAPI{}

	got, err := ensureScaleSet(context.Background(), api, iosScaleSet(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ensureScaleSet() error: %v", err)
	}
	if len(api.created) != 1 || got.ID != 900 {
		t.Fatalf("created %d scale sets, returned ID %d; want one created and returned", len(api.created), got.ID)
	}
	if c := api.created[0]; c.Name != "FrankMacStudioMacOS" || c.RunnerGroupID != 1 {
		t.Errorf("created %q in group %d, want FrankMacStudioMacOS in the default group 1", c.Name, c.RunnerGroupID)
	}
}

// A restart finds the scale set the previous run registered — jobs queued
// while runner was down are assigned to it — and must reuse it, not replace it.
func TestEnsureScaleSet_ReusesTheScaleSetARestartFinds(t *testing.T) {
	api := &fakeScaleSetAPI{existing: map[int]*scaleset.RunnerScaleSet{1: {ID: 181, Name: "FrankMacStudioMacOS"}}}

	got, err := ensureScaleSet(context.Background(), api, iosScaleSet(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("ensureScaleSet() error: %v", err)
	}
	if got.ID != 181 || !slices.Equal(api.updated, []int{181}) {
		t.Errorf("returned ID %d, updated %v; want scale set 181 reused and updated", got.ID, api.updated)
	}
	if len(api.created) != 0 || len(api.deleted) != 0 {
		t.Errorf("created %d and deleted %v on restart, want neither", len(api.created), api.deleted)
	}
}

func TestDeleteScaleSet_DeletesTheRegisteredScaleSet(t *testing.T) {
	api := &fakeScaleSetAPI{existing: map[int]*scaleset.RunnerScaleSet{1: {ID: 181, Name: "FrankMacStudioMacOS"}}}

	id, found, err := deleteScaleSet(context.Background(), api, iosScaleSet())
	if err != nil {
		t.Fatalf("deleteScaleSet() error: %v", err)
	}
	if !found || id != 181 || !slices.Equal(api.deleted, []int{181}) {
		t.Errorf("deleteScaleSet() = (%d, %v), deleted %v; want scale set 181 deleted", id, found, api.deleted)
	}
}

func TestDeleteScaleSet_NothingRegistered(t *testing.T) {
	api := &fakeScaleSetAPI{}

	_, found, err := deleteScaleSet(context.Background(), api, iosScaleSet())
	if err != nil {
		t.Fatalf("deleteScaleSet() error: %v", err)
	}
	if found || len(api.deleted) != 0 {
		t.Errorf("found = %v, deleted %v; want nothing found and nothing deleted", found, api.deleted)
	}
}

func TestDeleteScaleSet_LooksInTheConfiguredRunnerGroup(t *testing.T) {
	api := &fakeScaleSetAPI{
		groups:   map[string]int{"ios": 7},
		existing: map[int]*scaleset.RunnerScaleSet{7: {ID: 42, Name: "FrankMacStudioMacOS"}},
	}
	ss := iosScaleSet()
	ss.RunnerGroup = "ios"

	id, found, err := deleteScaleSet(context.Background(), api, ss)
	if err != nil {
		t.Fatalf("deleteScaleSet() error: %v", err)
	}
	if !slices.Equal(api.lookedUp, []int{7}) || !found || id != 42 {
		t.Errorf("looked up groups %v, got (%d, %v); want group 7 searched and scale set 42 deleted", api.lookedUp, id, found)
	}
}

// An unknown name is rejected from the config alone, before the machine lock
// or any call to GitHub.
func TestScalesetDeleteCommand_UnknownName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := `
[[scaleset]]
url = "https://github.com/re-tower"
name = "FrankMacStudioMacOS"
token = "ghp_test"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd.SetArgs([]string{"scaleset", "delete", "NoSuchScaleSet", "--config", path})
	defer cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "NoSuchScaleSet") {
		t.Fatalf("Execute() error = %v, want one naming the unknown scale set", err)
	}
}
