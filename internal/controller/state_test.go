package controller

import "github.com/ysya/runscaler/internal/capacity"

// State-only fixtures represent registered runners. Unknown-ID safety has
// dedicated readiness/drain tests using the real provisioning path.
func (r *instanceState) addIdle(name, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.instances[name] = &managedInstance{id: id, runnerID: 1, phase: instanceIdle, lease: &capacity.Lease{}}
}
