//go:build darwin && arm64

package provider

import (
	"context"
	"os"
	"os/exec"
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

	for _, scenario := range []struct {
		name string
		run  func(context.Context, *AppleContainerProvider, string)
	}{
		{name: "normal-job", run: func(ctx context.Context, p *AppleContainerProvider, id string) {
			stopLiveContainer(t, id)
			if err := p.RemoveInstance(ctx, id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "cancellation", run: func(ctx context.Context, p *AppleContainerProvider, id string) {
			stopLiveContainer(t, id)
			if err := p.WaitInstance(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err := p.RemoveInstance(ctx, id); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "restart-reconcile", run: func(ctx context.Context, p *AppleContainerProvider, id string) {
			stopLiveContainer(t, id)
			restarted := NewAppleContainerProvider(testAppleScaleSet(image, "live-apple-container"))
			if err := restarted.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			items, err := restarted.list(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.Configuration.ID == id {
					t.Fatalf("restart reconciliation left owned container %q", id)
				}
			}
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			p := NewAppleContainerProvider(testAppleScaleSet(image, "live-apple-container"))
			p.readyTimeout = 30 * time.Second
			p.poll = 200 * time.Millisecond
			id, err := p.StartInstance(ctx, "runscaler-live-apple-"+scenario.name, "synthetic-jit")
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
			scenario.run(ctx, p, id)
		})
	}
}

func stopLiveContainer(t *testing.T, id string) {
	t.Helper()
	if output, err := exec.Command("container", "stop", id).CombinedOutput(); err != nil {
		t.Fatalf("container stop %s: %v\n%s", id, err, output)
	}
}
