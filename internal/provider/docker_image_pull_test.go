package provider

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
)

type imagePullTestContextKey struct{}

type coordinatedImageDocker struct {
	*mockDocker
	mu              sync.Mutex
	present         bool
	pulls           int
	pullStarted     chan struct{}
	secondInspected chan struct{}
	release         chan struct{}
}

func (m *coordinatedImageDocker) ImageInspect(ctx context.Context, _ string, _ ...dockerclient.ImageInspectOption) (dockerclient.ImageInspectResult, error) {
	m.mu.Lock()
	present := m.present
	m.mu.Unlock()
	if present {
		return dockerclient.ImageInspectResult{}, nil
	}
	if ctx.Value(imagePullTestContextKey{}) == "second" {
		select {
		case m.secondInspected <- struct{}{}:
		default:
		}
	}
	return dockerclient.ImageInspectResult{}, cerrdefs.ErrNotFound
}

func (m *coordinatedImageDocker) ImagePull(ctx context.Context, _ string, _ dockerclient.ImagePullOptions) (dockerclient.ImagePullResponse, error) {
	m.mu.Lock()
	m.pulls++
	first := m.pulls == 1
	m.mu.Unlock()
	if first {
		close(m.pullStarted)
	}
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	m.mu.Lock()
	m.present = true
	m.mu.Unlock()
	return mockImagePullResponse{Reader: strings.NewReader("{}")}, nil
}

func TestDockerImagePullCoordinatorSharesPullAcrossScaleSets(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := &coordinatedImageDocker{
		mockDocker:      &mockDocker{},
		pullStarted:     make(chan struct{}),
		secondInspected: make(chan struct{}, 1),
		release:         make(chan struct{}),
	}
	ss := config.ScaleSetConfig{RunnerImage: "test-image:latest", Docker: config.DockerConfig{Socket: "/test/docker.sock"}}
	coordinator := NewDockerImagePullCoordinator()
	logger := slog.New(slog.DiscardHandler)
	first := NewDockerProviderWithCoordinator(ss, client, logger, coordinator)
	second := NewDockerProviderWithCoordinator(ss, client, logger, coordinator)

	firstDone := make(chan error, 1)
	go func() { firstDone <- first.ensureRunnerImage(ctx) }()
	select {
	case <-client.pullStarted:
	case <-ctx.Done():
		t.Fatal("first image pull did not start")
	}
	secondCtx := context.WithValue(ctx, imagePullTestContextKey{}, "second")
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.ensureRunnerImage(secondCtx) }()
	select {
	case <-client.secondInspected:
	case <-ctx.Done():
		t.Fatal("second scale set did not inspect the missing image")
	}
	close(client.release)
	for _, done := range []<-chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("ensureRunnerImage() error: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("image check did not complete")
		}
	}
	client.mu.Lock()
	pulls := client.pulls
	client.mu.Unlock()
	if pulls != 1 {
		t.Fatalf("image pulls = %d, want one shared pull", pulls)
	}
}

type observedDoneContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestDockerImagePullCoordinatorRetriesAfterLeaderCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	leaderCtx, cancelLeader := context.WithCancel(ctx)
	defer cancelLeader()
	coordinator := NewDockerImagePullCoordinator()
	key := dockerImagePullKey{socket: "/test/docker.sock", image: "test-image:latest"}
	started := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		leaderDone <- coordinator.ensure(leaderCtx, key, func() error {
			close(started)
			<-leaderCtx.Done()
			return leaderCtx.Err()
		})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("leader did not start")
	}
	followerCtx := &observedDoneContext{Context: ctx, observed: make(chan struct{})}
	followerDone := make(chan error, 1)
	go func() { followerDone <- coordinator.ensure(followerCtx, key, func() error { return nil }) }()
	select {
	case <-followerCtx.observed:
	case <-ctx.Done():
		t.Fatal("follower did not wait for the active pull")
	}
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want cancellation", err)
	}
	select {
	case err := <-followerDone:
		if err != nil {
			t.Fatalf("follower error = %v, want an independent retry", err)
		}
	case <-ctx.Done():
		t.Fatal("follower did not retry")
	}
}

func TestDockerImagePullCoordinatorKeepsDifferentTargetsIndependent(t *testing.T) {
	base := config.ScaleSetConfig{RunnerImage: "test-image:latest", Docker: config.DockerConfig{Socket: "/test/docker-a.sock"}}
	for _, tc := range []struct {
		name           string
		secondSocket   string
		secondImage    string
		firstPlatform  string
		secondPlatform string
	}{
		{name: "socket", secondSocket: "/test/docker-b.sock", secondImage: "test-image:latest"},
		{name: "image", secondSocket: "/test/docker-a.sock", secondImage: "other-image:latest"},
		{name: "platform", secondSocket: "/test/docker-a.sock", secondImage: "test-image:latest", firstPlatform: "linux/amd64", secondPlatform: "linux/arm64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			coordinator := NewDockerImagePullCoordinator()
			logger := slog.New(slog.DiscardHandler)
			firstConfig := base
			firstConfig.Docker.Platform = tc.firstPlatform
			secondConfig := base
			secondConfig.Docker.Socket = tc.secondSocket
			secondConfig.RunnerImage = tc.secondImage
			secondConfig.Docker.Platform = tc.secondPlatform
			first := NewDockerProviderWithCoordinator(firstConfig, &mockDocker{}, logger, coordinator)
			second := NewDockerProviderWithCoordinator(secondConfig, &mockDocker{}, logger, coordinator)
			started := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- coordinator.ensure(ctx, first.imagePullKey(), func() error {
					close(started)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("first target did not start")
			}
			secondDone := make(chan error, 1)
			go func() { secondDone <- coordinator.ensure(ctx, second.imagePullKey(), func() error { return nil }) }()
			select {
			case err := <-secondDone:
				if err != nil {
					t.Fatalf("second target error: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("different Docker target waited on the first pull")
			}
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatalf("first target error: %v", err)
			}
		})
	}
}
