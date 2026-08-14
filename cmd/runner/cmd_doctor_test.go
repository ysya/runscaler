package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"
)

// TestCheckDockerVolume_SharedVolumeIsNotOrphanedByExistence pins the core
// behavior change: the shared volume carries handoff data between jobs of
// one workflow run, so its mere existence must never be treated as a
// reason to remove it — not even under --fix.
func TestCheckDockerVolume_SharedVolumeIsNotOrphanedByExistence(t *testing.T) {
	md := &orphanFake{volumes: []string{"runner-shared"}}

	// fix=true must still not remove it: the volume is designed to outlive
	// the process, so its mere existence says nothing about whether it is
	// still needed.
	if _, err := checkDockerVolume(context.Background(), md, true); err != nil {
		t.Fatalf("checkDockerVolume: %v", err)
	}
	if len(md.volumesRemoved) != 0 {
		t.Errorf("doctor --fix removed %v; the shared volume carries handoff "+
			"data between jobs of one workflow run", md.volumesRemoved)
	}
}

// TestCheckDockerVolumeReportsCurrentAndLegacyNames covers both names
// sharedVolumeNames looks for existing at once: both are reported, neither
// is removed, and reporting an existing shared volume is never counted as
// an unresolved issue — it is expected steady state, not a problem.
func TestCheckDockerVolumeReportsCurrentAndLegacyNames(t *testing.T) {
	md := &orphanFake{volumes: []string{"runner-shared", "runscaler-shared"}}

	issues, err := checkDockerVolume(context.Background(), md, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0: an existing shared volume is expected state, not a problem", issues)
	}
	if len(md.volumesRemoved) != 0 {
		t.Errorf("expected no volumes removed, got %v", md.volumesRemoved)
	}
}

// TestCheckDockerVolumeReportsWithoutFix asserts fix=false behaves
// identically to fix=true (see TestCheckDockerVolumeReportsCurrentAndLegacyNames):
// this check no longer branches on fix at all.
func TestCheckDockerVolumeReportsWithoutFix(t *testing.T) {
	md := &orphanFake{volumes: []string{"runner-shared", "runscaler-shared"}}

	issues, err := checkDockerVolume(context.Background(), md, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0", issues)
	}
	if len(md.volumesRemoved) != 0 {
		t.Errorf("VolumeRemove must never be called by this check, got %v", md.volumesRemoved)
	}
}

func TestCheckDockerVolumeNoneFound(t *testing.T) {
	md := &orphanFake{}

	issues, err := checkDockerVolume(context.Background(), md, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0 when no volumes exist", issues)
	}
}

func TestCheckDockerVolumeSurfacesInspectFailure(t *testing.T) {
	md := &orphanFake{volumeInspectErr: fmt.Errorf("permission denied")}

	if _, err := checkDockerVolume(context.Background(), md, false); err == nil {
		t.Fatal("expected volume inspection failure to be returned")
	}
}

// TestVolumeSizeText pins "size not reported" for the cases doctor actually
// sees from a plain VolumeInspect call (UsageData is only populated by the
// disk-usage endpoint, so it is nil in practice) as well as the driver's
// explicit "-1 means unavailable" sentinel, and confirms a real size is
// formatted through backend.FormatBytes.
func TestVolumeSizeText(t *testing.T) {
	cases := []struct {
		name  string
		usage *volume.UsageData
		want  string
	}{
		{"nil usage data (plain inspect never returns it)", nil, "size not reported"},
		{"driver reports size unavailable (-1)", &volume.UsageData{Size: -1}, "size not reported"},
		{"known size", &volume.UsageData{Size: 2 * 1024 * 1024}, "2.0 MiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := dockerclient.VolumeInspectResult{Volume: volume.Volume{UsageData: tc.usage}}
			if got := volumeSizeText(result); got != tc.want {
				t.Errorf("volumeSizeText() = %q, want %q", got, tc.want)
			}
		})
	}
}
