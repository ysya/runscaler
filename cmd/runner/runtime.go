package main

import (
	"log/slog"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/capacity"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
)

// runtimeBackends is what startup connected for the configured providers.
type runtimeBackends struct {
	dockerClients map[string]*dockerclient.Client // keyed by Docker socket
	dockerPulls   *provider.DockerImagePullCoordinator
	tartHost      *provider.TartHostCoordinator // nil without Tart scale sets
}

// scaleSetRuntime is one scale set as the manager runs it: its resolved
// config plus what startup connected or detected for it.
type scaleSetRuntime struct {
	index    int
	config   config.ScaleSetConfig
	logger   *slog.Logger        // set by runManager before the controller starts
	capacity *capacity.Allocator // set by runManager when the broker exists

	// Exactly one is set.
	docker *dockerRuntime
	tart   *tartRuntime
}

type dockerRuntime struct {
	client *dockerclient.Client
	pulls  *provider.DockerImagePullCoordinator
}

type tartRuntime struct {
	host *provider.TartHostCoordinator
}

// newScaleSetRuntimes returns one runtime per scale set, in input order, with
// index set to its position. A Tart scale set carries only the shared Tart
// host; every other scale set carries the Docker client for its socket and
// the shared image pull coordinator. logger and capacity are left unset.
func newScaleSetRuntimes(scaleSets []config.ScaleSetConfig, b runtimeBackends) []*scaleSetRuntime {
	runtimes := make([]*scaleSetRuntime, len(scaleSets))
	for i, ss := range scaleSets {
		rt := &scaleSetRuntime{index: i, config: ss}
		if ss.IsTart() {
			rt.tart = &tartRuntime{host: b.tartHost}
		} else {
			rt.docker = &dockerRuntime{client: b.dockerClients[ss.Docker.Socket], pulls: b.dockerPulls}
		}
		runtimes[i] = rt
	}
	return runtimes
}
