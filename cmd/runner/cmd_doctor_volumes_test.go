package main

import (
	"context"
	"slices"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

func TestDoctorSharedVolumesFollowConfiguredNames(t *testing.T) {
	const rootless = "/run/user/1000/docker.sock"
	named := validScaleSet("named", 0)
	named.Docker.SharedVolume, named.Docker.SharedVolumeName = "/shared", "team-a"
	unnamed := validScaleSet("unnamed", 0)
	unnamed.Docker.Socket, unnamed.Docker.SharedVolume = rootless, "/shared"
	plain := validScaleSet("plain", 0)
	tart := validScaleSet("tart", 0)
	tart.Provider = "tart"

	got := doctorSharedVolumes(map[string]bool{config.DefaultDockerSocket: true, rootless: true}, []config.ScaleSetConfig{named, unnamed, plain, tart})
	if want := []string{"runner-shared", "team-a"}; !slices.Equal(got[config.DefaultDockerSocket], want) {
		t.Errorf("default socket volumes = %q, want %q", got[config.DefaultDockerSocket], want)
	}
	if want := []string{"runner-shared"}; !slices.Equal(got[rootless], want) {
		t.Errorf("rootless socket volumes = %q, want %q", got[rootless], want)
	}
}

func TestCheckDockerVolumeReportsAConfiguredName(t *testing.T) {
	md := &orphanFake{volumes: []string{"team-a"}}
	issues, err := checkDockerVolume(context.Background(), md, []string{"runner-shared", "team-a"})
	if err != nil || issues != 0 || len(md.volumesRemoved) != 0 {
		t.Fatalf("checkDockerVolume = %d, %v; removed %v", issues, err, md.volumesRemoved)
	}
}
