package cachestore

import "testing"

// TestDockerStoresUseConfiguredRootDir pins that the daemon stores take
// RootDir from their config, like every other Docker-backed store, so the
// caller resolves the daemon's data root once per socket instead of each
// store paying its own Info() round trip.
func TestDockerStoresUseConfiguredRootDir(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/from/info"}
	if got := ResolveDockerRootDir(fake); got != "/from/info" {
		t.Fatalf("ResolveDockerRootDir() = %q", got)
	}
	garbage := NewDockerGarbageStore(fake, DockerGarbageConfig{RootDir: "/var/lib/docker"})
	buildCache := NewDockerBuildCacheStore(fake, DockerBuildCacheConfig{RootDir: "/var/lib/docker"})
	if garbage.Path() != "/var/lib/docker" || buildCache.Path() != "/var/lib/docker" {
		t.Fatalf("Path() = %q, %q; want the configured root dir", garbage.Path(), buildCache.Path())
	}
	if fake.infoCalls != 1 {
		t.Fatalf("Info() called %d times; want once, by ResolveDockerRootDir only", fake.infoCalls)
	}
}
