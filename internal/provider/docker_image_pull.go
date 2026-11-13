package provider

import (
	"context"
	"errors"
	"sync"
)

type dockerImagePullKey struct {
	socket, image, os, architecture, variant string
}

type imagePull struct {
	done           chan struct{}
	err            error
	leaderCanceled bool
}

// DockerImagePullCoordinator shares in-flight pulls for the same Docker
// daemon, image, and platform.
type DockerImagePullCoordinator struct {
	mu    sync.Mutex
	pulls map[dockerImagePullKey]*imagePull
}

func NewDockerImagePullCoordinator() *DockerImagePullCoordinator {
	return &DockerImagePullCoordinator{}
}

func (p *DockerProvider) imagePullKey() dockerImagePullKey {
	key := dockerImagePullKey{socket: p.dockerSocket, image: p.runnerImage}
	if p.platform != nil {
		key.os = p.platform.OS
		key.architecture = p.platform.Architecture
		key.variant = p.platform.Variant
	}
	return key
}

func (c *DockerImagePullCoordinator) ensure(ctx context.Context, key dockerImagePullKey, run func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		if c.pulls == nil {
			c.pulls = make(map[dockerImagePullKey]*imagePull)
		}
		if pull, ok := c.pulls[key]; ok {
			c.mu.Unlock()
			select {
			case <-pull.done:
				if pull.leaderCanceled && ctx.Err() == nil {
					continue
				}
				return pull.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		pull := &imagePull{done: make(chan struct{})}
		c.pulls[key] = pull
		c.mu.Unlock()

		err := run()
		c.mu.Lock()
		pull.err = err
		pull.leaderCanceled = ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
		close(pull.done)
		delete(c.pulls, key)
		c.mu.Unlock()
		return err
	}
}
