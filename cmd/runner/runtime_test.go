package main

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/diagnostics"
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

// A dropped argument still builds a provider of the right type: the provider
// constructors fall back to a private coordinator when handed nil, and two
// Tart scale sets with private coordinators would try four VMs and both hand
// out the MAC of slot 0. The provider fields are unexported, so identity is
// read with reflect; a renamed field panics on the zero Value instead of
// passing.
func TestScaleSetRuntimeNewProvider_PassesSharedBackendsAndLogCapture(t *testing.T) {
	tartA, tartB := validScaleSet("tart-a", 0), validScaleSet("tart-b", 0)
	tartA.Provider, tartB.Provider = "tart", "tart"
	client := &dockerclient.Client{}
	b := runtimeBackends{
		dockerClients: map[string]*dockerclient.Client{config.DefaultDockerSocket: client},
		dockerPulls:   provider.NewDockerImagePullCoordinator(),
		tartHost:      provider.NewTartHostCoordinator(2),
	}
	runtimes := newScaleSetRuntimes([]config.ScaleSetConfig{tartA, tartB, validScaleSet("docker", 0)}, b)
	logger := slog.New(slog.DiscardHandler)
	for _, rt := range runtimes {
		rt.logger = logger
	}
	field := func(p any, name string) reflect.Value { return reflect.ValueOf(p).Elem().FieldByName(name) }

	for _, rt := range runtimes[:2] {
		p := rt.newProvider(context.Background(), managerDeps{})
		if field(p, "coordinator").Pointer() != reflect.ValueOf(b.tartHost).Pointer() {
			t.Errorf("%s: the Tart provider must share the host coordinator", rt.config.ScaleSetName)
		}
	}

	store := &diagnostics.Store{}
	for _, tc := range []struct {
		name        string
		deps        managerDeps
		wantCapture bool
	}{
		{name: "capture off without a store", deps: managerDeps{}},
		{name: "capture on without a store", deps: managerDeps{captureLogs: true}},
		{name: "capture off with a store", deps: managerDeps{diagnostics: store}},
		{name: "capture on with a store", deps: managerDeps{captureLogs: true, diagnostics: store}, wantCapture: true},
	} {
		p := runtimes[2].newProvider(context.Background(), tc.deps)
		if field(p, "dockerClient").Elem().Pointer() != reflect.ValueOf(client).Pointer() {
			t.Errorf("%s: the Docker provider must carry the client for its socket", tc.name)
		}
		if field(p, "imageCoordinator").Pointer() != reflect.ValueOf(b.dockerPulls).Pointer() {
			t.Errorf("%s: the Docker provider must share the image pull coordinator", tc.name)
		}
		if got := field(p, "captureLogs").Bool(); got != tc.wantCapture {
			t.Errorf("%s: captureLogs = %v, want %v", tc.name, got, tc.wantCapture)
		}
	}
}
