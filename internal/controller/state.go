package controller

import (
	"sync"
	"time"

	"github.com/ysya/runscaler/internal/capacity"
)

type instancePhase uint8

const (
	instanceProvisioning instancePhase = iota
	instanceIdle
	instanceBusy
	instanceRemoving
)

type managedInstance struct {
	id              string
	phase           instancePhase
	lease           *capacity.Lease
	cleanupInFlight bool
	cleanupAttempts int
	cleanupRetryAt  time.Time
}

// instanceState is the controller's source of truth. Provisioning instances
// count toward desired and host capacity before slow provider I/O begins;
// removing instances keep their lease until cleanup succeeds.
type instanceState struct {
	mu        sync.Mutex
	instances map[string]*managedInstance
}

func newInstanceState() instanceState {
	return instanceState{instances: make(map[string]*managedInstance)}
}

func (r *instanceState) reserve(name string, lease *capacity.Lease) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.instances[name]; exists {
		return false
	}
	r.instances[name] = &managedInstance{phase: instanceProvisioning, lease: lease}
	return true
}

// addIdle exists for state restoration and focused tests. Runtime scale-up
// uses reserve followed by markReady so provisioning is never invisible.
func (r *instanceState) addIdle(name, instanceID string) {
	r.mu.Lock()
	r.instances[name] = &managedInstance{id: instanceID, phase: instanceIdle, lease: &capacity.Lease{}}
	r.mu.Unlock()
}

func (r *instanceState) failProvision(name string) {
	r.mu.Lock()
	if instance, ok := r.instances[name]; ok && instance.id == "" {
		delete(r.instances, name)
	}
	r.mu.Unlock()
}

// markReady records the provider ID. removeNow is true when a completion or
// drain raced provider startup and cleanup must happen immediately.
func (r *instanceState) markReady(name, instanceID string) (removeNow, tracked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok {
		return false, false
	}
	instance.id = instanceID
	if instance.phase == instanceProvisioning {
		instance.phase = instanceIdle
	}
	return instance.phase == instanceRemoving, true
}

// count returns target-bearing instances. Removing instances are excluded so
// desired capacity can be replaced as soon as their global lease is released.
func (r *instanceState) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, instance := range r.instances {
		if instance.phase != instanceRemoving {
			count++
		}
	}
	return count
}

func (r *instanceState) counts() (idle, busy int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, instance := range r.instances {
		switch instance.phase {
		case instanceIdle:
			idle++
		case instanceBusy:
			busy++
		}
	}
	return idle, busy
}

func (r *instanceState) lifecycleCounts() (provisioning, idle, busy, removing int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, instance := range r.instances {
		switch instance.phase {
		case instanceProvisioning:
			provisioning++
		case instanceIdle:
			idle++
		case instanceBusy:
			busy++
		case instanceRemoving:
			removing++
		}
	}
	return provisioning, idle, busy, removing
}

func (r *instanceState) phase(name string) (instancePhase, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok {
		return 0, false
	}
	return instance.phase, true
}

func (r *instanceState) contains(name, instanceID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	return ok && instance.id == instanceID && instance.phase != instanceRemoving
}

func (r *instanceState) markBusy(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok || (instance.phase != instanceIdle && instance.phase != instanceProvisioning) {
		return false
	}
	instance.phase = instanceBusy
	return true
}

// markDone transitions a runner to removing. ready is false only for the
// narrow race where GitHub reports completion before StartInstance returns.
func (r *instanceState) markDone(name string) (instanceID string, ready, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok || instance.phase == instanceRemoving {
		return "", false, false
	}
	instance.phase = instanceRemoving
	return instance.id, instance.id != "", true
}

func (r *instanceState) markDead(name, instanceID string) (*capacity.Lease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok || instance.id != instanceID || instance.phase == instanceRemoving {
		return nil, false
	}
	delete(r.instances, name)
	return instance.lease, true
}

// markIdleAboveTarget transitions one surplus idle instance to removing. Busy
// runners are never preempted, and the count/transition share the same lock as
// markBusy so job assignment wins or loses the boundary atomically.
func (r *instanceState) markIdleAboveTarget(target int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := 0
	for _, instance := range r.instances {
		if instance.phase != instanceRemoving {
			current++
		}
	}
	if current <= target {
		return false
	}
	for _, instance := range r.instances {
		if instance.phase == instanceIdle {
			instance.phase = instanceRemoving
			return true
		}
	}
	return false
}

// idleForRemoval atomically establishes the drain boundary by transitioning
// every idle runner to removing before returning provider IDs.
func (r *instanceState) idleForRemoval() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]string)
	for name, instance := range r.instances {
		if instance.phase == instanceIdle {
			instance.phase = instanceRemoving
			result[name] = instance.id
		}
	}
	return result
}

// claimRemoval atomically assigns one eligible cleanup operation to a worker.
// A failed cleanup is given its own retry deadline, so it cannot block cleanup
// or scale-up work for unrelated instances.
func (r *instanceState) claimRemoval(now time.Time) (name, instanceID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, instance := range r.instances {
		if instance.phase == instanceRemoving && instance.id != "" && !instance.cleanupInFlight && !now.Before(instance.cleanupRetryAt) {
			instance.cleanupInFlight = true
			return name, instance.id, true
		}
	}
	return "", "", false
}

func (r *instanceState) retryRemoval(name, instanceID string, now time.Time) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok || instance.phase != instanceRemoving || instance.id != instanceID || !instance.cleanupInFlight {
		return 0, false
	}
	instance.cleanupInFlight = false
	instance.cleanupAttempts++
	delay := 5 * time.Second
	for range min(instance.cleanupAttempts-1, 4) {
		delay *= 2
	}
	instance.cleanupRetryAt = now.Add(delay)
	return delay, true
}

func (r *instanceState) finishRemoval(name, instanceID string) (*capacity.Lease, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	instance, ok := r.instances[name]
	if !ok || instance.phase != instanceRemoving || instance.id != instanceID {
		return nil, false
	}
	delete(r.instances, name)
	return instance.lease, true
}

func (r *instanceState) detachAll() map[string]managedInstance {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]managedInstance, len(r.instances))
	for name, instance := range r.instances {
		result[name] = *instance
	}
	clear(r.instances)
	return result
}
