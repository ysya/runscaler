package backend

import "context"

// RunnerBackend abstracts runner lifecycle operations across different
// execution backends (Docker containers, Tart VMs, etc.).
type RunnerBackend interface {
	// StartRunner creates and starts a new ephemeral runner with the given
	// name and JIT configuration. Returns a resource ID used for cleanup.
	StartRunner(ctx context.Context, name string, jitConfig string) (resourceID string, err error)

	// RemoveRunner stops and removes a runner by its resource ID.
	RemoveRunner(ctx context.Context, resourceID string) error

	// Shutdown performs backend-specific cleanup (prune images, remove volumes, etc.).
	Shutdown(ctx context.Context)
}
