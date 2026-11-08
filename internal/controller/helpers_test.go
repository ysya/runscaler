package controller

import "context"

// Test-only shortcuts. Production code holds a capacity lease before it
// starts a runner (startInstanceWithLease) and reads draining and phase
// state through RuntimeStatus and the lifecycle counters.

// startInstance acquires a lease and starts one runner.
func (s *ScaleSetController) startInstance(ctx context.Context) (string, error) {
	lease, err := s.capacity.Acquire(ctx)
	if err != nil {
		return "", err
	}
	return s.startInstanceWithLease(ctx, lease)
}

// IsDraining reports whether the controller has stopped accepting new work.
func (s *ScaleSetController) IsDraining() bool {
	return s.draining.Load()
}

// phase returns the recorded lifecycle phase of a runner.
func (r *instanceState) phase(name string) (instancePhase, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok {
		return 0, false
	}
	return instance.phase, true
}
