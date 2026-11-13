//go:build darwin && arm64

package controller

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"log/slog"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
)

type liveAppleScaleSetClient struct{ nextID int64 }

func (c *liveAppleScaleSetClient) GenerateJitRunnerConfig(context.Context, *scaleset.RunnerScaleSetJitRunnerSetting, int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	c.nextID++
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: int(c.nextID)},
		EncodedJITConfig: "synthetic-jit-config",
	}, nil
}

func (*liveAppleScaleSetClient) RemoveRunner(context.Context, int64) error { return nil }

func TestAppleContainerControllerLiveWithMockedGitHub(t *testing.T) {
	image := os.Getenv("RUNSCALER_APPLE_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set RUNSCALER_APPLE_CONTAINER_IMAGE to run the Apple Container controller test")
	}

	p := provider.NewAppleContainerProvider(config.ScaleSetConfig{
		ScaleSetName: "live-controller-apple",
		RunnerImage:  image,
		Provider:     "apple-container",
		AppleContainer: config.AppleContainerConfig{
			CPU:    2,
			Memory: 1024,
		},
	})
	client := &liveAppleScaleSetClient{}
	s := NewScaleSetController(1, 0, 1, p, client, slog.New(slog.DiscardHandler))
	defer s.Shutdown(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := s.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatal(err)
	}
	waitForAppleController(t, ctx, func() bool {
		idle, _ := s.InstanceCounts()
		return idle == 1
	})

	runnerName := liveAppleRunnerName(s)
	if runnerName == "" {
		t.Fatal("controller reached idle state without a runner name")
	}
	if err := s.HandleJobStarted(ctx, &scaleset.JobStarted{RunnerName: runnerName, JobMessageBase: scaleset.JobMessageBase{JobID: "mock-job"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.HandleJobCompleted(ctx, &scaleset.JobCompleted{RunnerName: runnerName, JobMessageBase: scaleset.JobMessageBase{JobID: "mock-job"}}); err != nil {
		t.Fatal(err)
	}
	waitForAppleController(t, ctx, func() bool {
		idle, busy := s.InstanceCounts()
		return idle == 0 && busy == 0
	})
}

func liveAppleRunnerName(s *ScaleSetController) string {
	s.instances.mu.Lock()
	defer s.instances.mu.Unlock()
	for name := range s.instances.instances {
		return name
	}
	return ""
}

func waitForAppleController(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
