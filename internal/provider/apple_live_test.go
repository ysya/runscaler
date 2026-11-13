//go:build darwin && arm64

package provider

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ysya/runscaler/internal/config"
)

func testAppleScaleSet(image, name string) config.ScaleSetConfig {
	return config.ScaleSetConfig{
		ScaleSetName: name,
		RunnerImage:  image,
		Provider:     "apple-container",
		AppleContainer: config.AppleContainerConfig{
			CPU:    2,
			Memory: 1024,
		},
	}
}

func TestAppleContainerProvider_LiveLifecycle(t *testing.T) {
	image := os.Getenv("RUNSCALER_APPLE_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set RUNSCALER_APPLE_CONTAINER_IMAGE to run the Apple Container lifecycle test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p := NewAppleContainerProvider(testAppleScaleSet(image, "live-apple-container"))
	p.readyTimeout = 30 * time.Second
	p.poll = 200 * time.Millisecond

	id, err := p.StartInstance(ctx, "runscaler-live-apple", "synthetic-jit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = p.RemoveInstance(cleanupCtx, id)
	})

	if err := p.WaitReady(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveInstance(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitInstance(ctx, id); err != nil {
		t.Fatal(err)
	}
}
