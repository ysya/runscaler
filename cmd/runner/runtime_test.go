package main

import (
	"context"
	"log/slog"
	"testing"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
)

func TestNewScaleSetRuntimes_AssignsProviderSpecificParts(t *testing.T) {
	dockerA := validScaleSet("docker-a", 0)
	dockerB := validScaleSet("docker-b", 1)
	dockerB.Docker.Socket = "/run/other.sock"
	dockerC := validScaleSet("docker-c", 0) // shares docker-a's socket
	tart := validScaleSet("tart", 0)
	tart.Provider = "tart"
	clientA, clientB := &dockerclient.Client{}, &dockerclient.Client{}
	b := runtimeBackends{
		dockerClients: map[string]*dockerclient.Client{config.DefaultDockerSocket: clientA, "/run/other.sock": clientB},
		dockerPulls:   provider.NewDockerImagePullCoordinator(),
		tartHost:      provider.NewTartHostCoordinator(2),
	}

	runtimes := newScaleSetRuntimes([]config.ScaleSetConfig{dockerA, dockerB, dockerC, tart}, b)

	if len(runtimes) != 4 {
		t.Fatalf("got %d runtimes, want 4", len(runtimes))
	}
	for i, name := range []string{"docker-a", "docker-b", "docker-c", "tart"} {
		if runtimes[i].index != i || runtimes[i].config.ScaleSetName != name {
			t.Errorf("runtimes[%d] = index %d, name %q; want index %d, name %q", i, runtimes[i].index, runtimes[i].config.ScaleSetName, i, name)
		}
	}
	for i, want := range []*dockerclient.Client{clientA, clientB, clientA} {
		rt := runtimes[i]
		if rt.tart != nil || rt.docker == nil || rt.docker.client != want || rt.docker.pulls != b.dockerPulls {
			t.Errorf("runtimes[%d]: docker part does not carry the client for socket %q", i, rt.config.Docker.Socket)
		}
	}
	if rt := runtimes[3]; rt.docker != nil || rt.tart == nil || rt.tart.host != b.tartHost {
		t.Error("the Tart scale set must carry only the shared Tart host")
	}
}

func TestScaleSetRuntimeNewProvider_SelectsByProviderKind(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	tart := validScaleSet("tart", 0)
	tart.Provider = "tart"
	tartRT := &scaleSetRuntime{config: tart, logger: logger, tart: &tartRuntime{host: provider.NewTartHostCoordinator(2)}}
	if _, ok := tartRT.newProvider(context.Background(), managerDeps{}).(*provider.TartProvider); !ok {
		t.Error("a Tart runtime must build a TartProvider")
	}

	dockerRT := &scaleSetRuntime{
		config: validScaleSet("docker", 0),
		logger: logger,
		docker: &dockerRuntime{client: &dockerclient.Client{}, pulls: provider.NewDockerImagePullCoordinator()},
	}
	if _, ok := dockerRT.newProvider(context.Background(), managerDeps{}).(*provider.DockerProvider); !ok {
		t.Error("a Docker runtime must build a DockerProvider")
	}
}
