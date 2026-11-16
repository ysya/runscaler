package main

import (
	"context"
	"slices"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

func TestPrepareRunnerImagesIncludesPrivateDaemons(t *testing.T) {
	a := validScaleSet("a", 0)
	a.RunnerImage = "runner:test"
	a.Docker.DaemonMode = "dind"
	b := a
	b.Docker.Platform = "linux/arm64"
	b.Docker.DinDImage = "custom:dind"
	var got []string
	err := prepareRunnerImages(context.Background(), []config.ScaleSetConfig{a, a, b}, func(_ context.Context, ss config.ScaleSetConfig) error {
		got = append(got, ss.RunnerImage+"|"+ss.Docker.Platform)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"runner:test|", "docker:29-dind|", "runner:test|linux/arm64", "custom:dind|linux/arm64"}) {
		t.Fatalf("missing or duplicated required images: %v", got)
	}
}
