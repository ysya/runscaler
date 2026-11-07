package provider

import (
	"context"
	"errors"
	"time"
)

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

// InstanceReadiness is optional. Resource creation and readiness are distinct:
// a returned ID stays tracked until readiness, work evidence or safe cleanup.
type InstanceReadiness interface {
	WaitReady(context.Context, string) error
}

var (
	ErrReadinessDisabled    = errors.New("readiness_disabled")
	ErrReadinessTimeout     = errors.New("readiness_timeout")
	ErrReadinessUnavailable = errors.New("readiness_unavailable")
	ErrExitedBeforeReady    = errors.New("runner_exited_before_ready")
)

type ExitDiagnostics struct {
	ExitCode   *int       `json:"exit_code,omitempty"`
	OOMKilled  *bool      `json:"oom_killed,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// RawLog is private collection input. It is never serialized directly.
	RawLog string `json:"-"`
}

type InstanceDiagnostics interface {
	CollectDiagnostics(context.Context, string) (ExitDiagnostics, error)
}
