package main

import (
	"context"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	dockerclient "github.com/moby/moby/client"
)

type fakeVolumeAPI struct {
	existing   map[string]bool
	removed    []string
	inspectErr error
}

func (f *fakeVolumeAPI) VolumeInspect(_ context.Context, id string, _ dockerclient.VolumeInspectOptions) (dockerclient.VolumeInspectResult, error) {
	if f.existing[id] {
		return dockerclient.VolumeInspectResult{}, nil
	}
	if f.inspectErr != nil {
		return dockerclient.VolumeInspectResult{}, f.inspectErr
	}
	return dockerclient.VolumeInspectResult{}, cerrdefs.ErrNotFound
}

func (f *fakeVolumeAPI) VolumeRemove(_ context.Context, id string, _ dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	f.removed = append(f.removed, id)
	return dockerclient.VolumeRemoveResult{}, nil
}

func TestCheckDockerVolumeRemovesCurrentAndLegacy(t *testing.T) {
	f := &fakeVolumeAPI{existing: map[string]bool{
		"runner-shared":    true,
		"runscaler-shared": true,
	}}
	issues, err := checkDockerVolume(context.Background(), f, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0 after fixing", issues)
	}
	if len(f.removed) != 2 {
		t.Fatalf("expected both volumes removed, got %v", f.removed)
	}
	removed := map[string]bool{}
	for _, v := range f.removed {
		removed[v] = true
	}
	for _, want := range []string{"runner-shared", "runscaler-shared"} {
		if !removed[want] {
			t.Errorf("expected %q to be removed, got %v", want, f.removed)
		}
	}
}

func TestCheckDockerVolumeReportsWithoutFix(t *testing.T) {
	f := &fakeVolumeAPI{existing: map[string]bool{
		"runner-shared":    true,
		"runscaler-shared": true,
	}}
	issues, err := checkDockerVolume(context.Background(), f, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 2 {
		t.Errorf("issues = %d, want 2 when reporting without fix", issues)
	}
	if len(f.removed) != 0 {
		t.Errorf("VolumeRemove must not be called when fix=false, got %v", f.removed)
	}
}

func TestCheckDockerVolumeNoneFound(t *testing.T) {
	f := &fakeVolumeAPI{existing: map[string]bool{}}
	issues, err := checkDockerVolume(context.Background(), f, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0 when no volumes exist", issues)
	}
}

func TestCheckDockerVolumeSurfacesInspectFailure(t *testing.T) {
	f := &fakeVolumeAPI{existing: map[string]bool{}, inspectErr: fmt.Errorf("permission denied")}
	if _, err := checkDockerVolume(context.Background(), f, false); err == nil {
		t.Fatal("expected volume inspection failure to be returned")
	}
}
