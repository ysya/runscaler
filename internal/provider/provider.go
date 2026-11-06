package provider

import "context"

// InstanceProvider provisions and removes the isolated instance that hosts
// one ephemeral GitHub Actions runner (a Docker container or Tart VM).
type InstanceProvider interface {
	// StartInstance creates and starts a new ephemeral runner with the given
	// name and JIT configuration. Returns an instance ID used for cleanup.
	StartInstance(ctx context.Context, name string, jitConfig string) (instanceID string, err error)

	// RemoveInstance stops and removes a runner by its instance ID. It must be
	// idempotent (an already-absent instance is success) and must honor ctx so
	// controller drain and shutdown deadlines remain bounded.
	RemoveInstance(ctx context.Context, instanceID string) error

	// Shutdown performs provider-specific cleanup.
	Shutdown(ctx context.Context)
}

// InstanceWatcher is optionally implemented by providers that can observe an
// instance exiting independently of GitHub job messages. A nil return means
// the instance stopped; a non-nil error means observation failed or the
// context ended.
type InstanceWatcher interface {
	WaitInstance(ctx context.Context, instanceID string) error
}
