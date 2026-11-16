package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
)

// The fake models Docker's external resource state, including volume references
// and wait cancellation. Provider provisioning and cleanup remain real.
type dinDFake struct {
	*mockDocker
	mu       sync.Mutex
	objects  map[string]dockerclient.ContainerCreateOptions
	stores   map[string]volume.Volume
	stopped  map[string]bool
	exits    map[string]chan struct{}
	events   []string
	fail     string
	starting bool
	initExit int64
	images   map[string]bool
	pulls    []string
}

func (m *dinDFake) ImageInspect(ctx context.Context, image string, options ...dockerclient.ImageInspectOption) (dockerclient.ImageInspectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.images == nil {
		return m.mockDocker.ImageInspect(ctx, image, options...)
	}
	if !m.images[image] {
		return dockerclient.ImageInspectResult{}, cerrdefs.ErrNotFound
	}
	return dockerclient.ImageInspectResult{}, ctx.Err()
}

func (m *dinDFake) ImagePull(ctx context.Context, image string, options dockerclient.ImagePullOptions) (dockerclient.ImagePullResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.images == nil {
		return m.mockDocker.ImagePull(ctx, image, options)
	}
	m.pulls = append(m.pulls, image)
	m.images[image] = true
	return mockImagePullResponse{Reader: strings.NewReader("{}")}, ctx.Err()
}

func TestDinDRepairsPrunedDaemonImageAcrossScaleSets(t *testing.T) {
	m := newDinDFake()
	m.images = map[string]bool{"runner:test": true}
	p1, p2 := newDinDTestProvider(m), newDinDTestProvider(m)
	shared := NewDockerImagePullCoordinator()
	p1.imageCoordinator, p2.imageCoordinator = shared, shared
	var wg sync.WaitGroup
	for i, p := range []*DockerProvider{p1, p2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.StartInstance(context.Background(), fmt.Sprintf("repair-%d", i), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if !slices.Equal(m.pulls, []string{"docker:29-dind"}) {
		t.Fatalf("daemon image recovery was missing or duplicated: %v", m.pulls)
	}
	groups, err := FindDinDOrphans(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if err := RemoveDinDInstance(context.Background(), m, group, slog.Default()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDinDPreservesNativeActionsCacheGuard(t *testing.T) {
	m := newDinDFake()
	p := NewDockerProvider(config.ScaleSetConfig{RunnerImage: "runner:test", Docker: config.DockerConfig{DaemonMode: "dind", ActionsCacheURL: "http://cache.example.test"}}, m, slog.New(slog.DiscardHandler))
	id, err := p.StartInstance(context.Background(), "native-cache", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := m.objects[id]
	if !strings.Contains(strings.Join(runner.Config.Cmd, " "), "/usr/local/bin/runner-local-cache-check &&") {
		t.Fatal("DinD bypassed native Actions cache guard")
	}
	if !slices.Contains(runner.Config.Env, "CUSTOM_ACTIONS_RESULTS_URL=http://cache.example.test") {
		t.Fatal("DinD discarded native cache endpoint")
	}
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func newDinDFake() *dinDFake {
	return &dinDFake{mockDocker: &mockDocker{imagePresent: true}, objects: map[string]dockerclient.ContainerCreateOptions{}, stores: map[string]volume.Volume{}, stopped: map[string]bool{}, exits: map[string]chan struct{}{}}
}

func (m *dinDFake) ContainerCreate(ctx context.Context, o dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	role := o.Config.Labels["runscaler.dind-role"]
	m.events = append(m.events, "create:"+role)
	if m.fail == "create:"+role {
		return dockerclient.ContainerCreateResult{}, errors.New("injected create failure")
	}
	if err := ctx.Err(); err != nil {
		return dockerclient.ContainerCreateResult{}, err
	}
	id := "id-" + o.Name
	m.objects[id] = o
	m.exits[id] = make(chan struct{})
	return dockerclient.ContainerCreateResult{ID: id}, nil
}

func (m *dinDFake) ContainerStart(ctx context.Context, id string, _ dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	role := m.objects[id].Config.Labels["runscaler.dind-role"]
	m.events = append(m.events, "start:"+role)
	if m.fail == "start:"+role {
		return dockerclient.ContainerStartResult{}, errors.New("injected start failure")
	}
	return dockerclient.ContainerStartResult{}, ctx.Err()
}

func (m *dinDFake) ContainerInspect(ctx context.Context, id string, _ dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[id]
	if !ok {
		return dockerclient.ContainerInspectResult{}, cerrdefs.ErrNotFound
	}
	role := o.Config.Labels["runscaler.dind-role"]
	m.events = append(m.events, "inspect:"+role)
	if m.fail == "inspect:"+role {
		return dockerclient.ContainerInspectResult{}, errors.New("injected inspect failure")
	}
	health := container.Healthy
	if m.starting {
		health = container.Starting
	}
	return dockerclient.ContainerInspectResult{Container: container.InspectResponse{ID: id, Config: o.Config, State: &container.State{Running: !m.stopped[id], Health: &container.Health{Status: health}}}}, ctx.Err()
}

func (m *dinDFake) ContainerStop(ctx context.Context, id string, _ dockerclient.ContainerStopOptions) (dockerclient.ContainerStopResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[id]
	if !ok {
		return dockerclient.ContainerStopResult{}, cerrdefs.ErrNotFound
	}
	m.events = append(m.events, "stop:"+o.Config.Labels["runscaler.dind-role"])
	if !m.stopped[id] {
		m.stopped[id] = true
		close(m.exits[id])
	}
	return dockerclient.ContainerStopResult{}, ctx.Err()
}

func (m *dinDFake) ContainerRemove(ctx context.Context, id string, _ dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[id]
	if !ok {
		return dockerclient.ContainerRemoveResult{}, cerrdefs.ErrNotFound
	}
	role := o.Config.Labels["runscaler.dind-role"]
	m.events = append(m.events, "remove:"+role)
	if m.fail == "remove:"+role {
		return dockerclient.ContainerRemoveResult{}, errors.New("injected removal failure")
	}
	if !m.stopped[id] {
		m.stopped[id] = true
		close(m.exits[id])
	}
	delete(m.objects, id)
	return dockerclient.ContainerRemoveResult{}, ctx.Err()
}

func (m *dinDFake) ContainerList(ctx context.Context, _ dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result dockerclient.ContainerListResult
	for id, o := range m.objects {
		c := container.Summary{ID: id, Names: []string{"/" + o.Name}, Labels: o.Config.Labels}
		c.HostConfig.NetworkMode = string(o.HostConfig.NetworkMode)
		for _, v := range o.HostConfig.Mounts {
			c.Mounts = append(c.Mounts, container.MountPoint{Type: v.Type, Name: v.Source, Destination: v.Target})
		}
		result.Items = append(result.Items, c)
	}
	return result, ctx.Err()
}

func (m *dinDFake) VolumeCreate(ctx context.Context, o dockerclient.VolumeCreateOptions) (dockerclient.VolumeCreateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail == "volume" {
		return dockerclient.VolumeCreateResult{}, errors.New("injected volume failure")
	}
	v := volume.Volume{Name: o.Name, Labels: o.Labels}
	m.stores[o.Name] = v
	return dockerclient.VolumeCreateResult{Volume: v}, ctx.Err()
}

func (m *dinDFake) VolumeList(ctx context.Context, _ dockerclient.VolumeListOptions) (dockerclient.VolumeListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var r dockerclient.VolumeListResult
	for _, v := range m.stores {
		r.Items = append(r.Items, v)
	}
	return r, ctx.Err()
}

func (m *dinDFake) VolumeRemove(ctx context.Context, name string, o dockerclient.VolumeRemoveOptions) (dockerclient.VolumeRemoveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o.Force {
		return dockerclient.VolumeRemoveResult{}, errors.New("must not force volume removal")
	}
	if m.fail == "volume-remove" {
		return dockerclient.VolumeRemoveResult{}, errors.New("injected volume removal failure")
	}
	for _, c := range m.objects {
		for _, v := range c.HostConfig.Mounts {
			if v.Source == name {
				return dockerclient.VolumeRemoveResult{}, errors.New("volume still in use")
			}
		}
	}
	delete(m.stores, name)
	return dockerclient.VolumeRemoveResult{}, ctx.Err()
}

func (m *dinDFake) ContainerWait(ctx context.Context, id string, _ dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult {
	r := make(chan container.WaitResponse, 1)
	e := make(chan error, 1)
	m.mu.Lock()
	o, ok := m.objects[id]
	exit := m.exits[id]
	status := m.initExit
	failure := m.fail
	m.mu.Unlock()
	if !ok {
		e <- cerrdefs.ErrNotFound
	} else if failure == "wait:daemon" && o.Config.Labels[dindRoleLabel] == "daemon" {
		e <- errors.New("injected monitor failure")
	} else if o.Config.Labels["runscaler.dind-role"] == "init" {
		r <- container.WaitResponse{StatusCode: status}
	} else {
		go func() {
			select {
			case <-exit:
				r <- container.WaitResponse{}
			case <-ctx.Done():
				e <- ctx.Err()
			}
		}()
	}
	return dockerclient.ContainerWaitResult{Result: r, Error: e}
}

func newDinDTestProvider(m *dinDFake) *DockerProvider {
	return NewDockerProvider(config.ScaleSetConfig{RunnerImage: "runner:test", Docker: config.DockerConfig{DaemonMode: "dind", SharedVolume: "/shared", SharedVolumeName: "persistent-shared", CacheVolumes: []string{"persistent-cache:/home/runner/.cache"}}}, m, slog.New(slog.DiscardHandler))
}

func TestDinDInstancesHavePrivateDaemonsAndStorage(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	var ids []string
	for _, name := range []string{"job-a", "job-b"} {
		id, err := p.StartInstance(context.Background(), name, "private-jit")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	groups := map[string]bool{}
	data := map[string]bool{}
	for id, o := range m.objects {
		role := o.Config.Labels["runscaler.dind-role"]
		group := o.Config.Labels["runscaler.dind-instance"]
		groups[group] = true
		for _, v := range o.HostConfig.Mounts {
			if v.Type == mount.TypeBind {
				t.Fatalf("outer bind leaked into %s: %+v", role, v)
			}
			if v.Target == "/var/lib/docker" {
				data[v.Source] = true
			}
		}
		switch role {
		case "runner":
			if o.HostConfig.Privileged {
				t.Fatal("runner must remain unprivileged")
			}
			if o.HostConfig.CgroupnsMode != "private" {
				t.Fatal("runner must not expose host cgroup paths to GitHub container compatibility checks")
			}
			if !strings.HasPrefix(string(o.HostConfig.NetworkMode), "container:") {
				t.Fatal("runner does not share its daemon network")
			}
			if !slices.Contains(o.Config.Env, "DOCKER_HOST=unix:///var/run/docker.sock") {
				t.Fatal("missing private socket environment")
			}
			if !slices.Contains(o.Config.Env, "ACTIONS_RUNNER_INPUT_JITCONFIG=private-jit") {
				t.Fatal("missing runner JIT")
			}
		case "daemon":
			if !o.HostConfig.Privileged {
				t.Fatal("daemon is not privileged")
			}
			if !slices.Equal([]string(o.Config.Cmd), []string{"dockerd", "--host=unix:///var/run/docker.sock", "--group=123"}) {
				t.Fatalf("unexpected daemon listeners: %v", o.Config.Cmd)
			}
			if strings.Contains(strings.Join(o.Config.Env, " "), "private-jit") {
				t.Fatal("JIT leaked to daemon")
			}
		default:
			t.Fatalf("initializer survived successful start: %s %s", id, role)
		}
	}
	if len(groups) != 2 || len(data) != 2 {
		t.Fatalf("instances share state: groups=%v data=%v", groups, data)
	}
	ready := slices.Index(m.events, "inspect:daemon")
	runner := slices.Index(m.events, "create:runner")
	if ready < 0 || runner < ready {
		t.Fatalf("runner started before daemon readiness: %v", m.events)
	}
	for _, id := range ids {
		if err := p.RemoveInstance(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.objects) != 0 || len(m.stores) != 0 {
		t.Fatalf("leaked resources: %v %v", m.objects, m.stores)
	}
}

func TestDinDStartupRollsBackEveryFailure(t *testing.T) {
	for _, failure := range []string{"volume", "create:init", "start:init", "init-exit", "create:daemon", "start:daemon", "inspect:daemon", "create:runner", "start:runner", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			m := newDinDFake()
			m.fail = failure
			if failure == "init-exit" {
				m.initExit = 1
			}
			p := newDinDTestProvider(m)
			ctx := context.Background()
			if failure == "timeout" {
				m.starting = true
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 15*time.Millisecond)
				defer cancel()
			}
			if _, err := p.StartInstance(ctx, "failure-test", "private-jit"); err == nil {
				t.Fatal("startup unexpectedly succeeded")
			}
			if len(m.objects) != 0 || len(m.stores) != 0 {
				t.Fatalf("rollback leaked resources: %d containers, %d volumes", len(m.objects), len(m.stores))
			}
		})
	}
}

func TestDinDCleanupRecoversAfterRunnerDisappears(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	id, err := p.StartInstance(context.Background(), "lost-runner", "")
	if err != nil {
		t.Fatal(err)
	}
	delete(m.objects, id)
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(m.objects) != 0 || len(m.stores) != 0 {
		t.Fatalf("resources stranded after runner removal: %v %v", m.objects, m.stores)
	}
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestDinDOrphansIncludeVolumeOnlyAndPreserveUnrelatedResources(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	id, err := p.StartInstance(context.Background(), "orphan", "")
	if err != nil {
		t.Fatal(err)
	}
	group := m.objects[id].Config.Labels["runscaler.dind-instance"]
	for id := range m.objects {
		delete(m.objects, id)
	}
	m.stores["persistent-cache"] = volume.Volume{Name: "persistent-cache"}
	m.stores["foreign"] = volume.Volume{Name: "foreign", Labels: map[string]string{"runscaler.dind-instance": group}}
	groups, err := FindDinDOrphans(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups, []string{group}) {
		t.Fatalf("volume-only group not discovered: %v", groups)
	}
	if err := RemoveDinDInstance(context.Background(), m, group, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if len(m.stores) != 2 {
		t.Fatalf("unrelated volumes deleted or owned volumes leaked: %v", m.stores)
	}
}

func TestDinDRemovalFailureRetainsOwnershipForRetry(t *testing.T) {
	for _, failure := range []string{"remove:daemon", "volume-remove"} {
		t.Run(failure, func(t *testing.T) {
			m := newDinDFake()
			p := newDinDTestProvider(m)
			id, err := p.StartInstance(context.Background(), "retry", "")
			if err != nil {
				t.Fatal(err)
			}
			m.fail = failure
			if err := p.RemoveInstance(context.Background(), id); err == nil {
				t.Fatal("cleanup failure hidden")
			}
			m.fail = ""
			if err := p.RemoveInstance(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if len(m.objects) != 0 || len(m.stores) != 0 {
				t.Fatal("retry left resources")
			}
		})
	}
}

func TestDinDDaemonExitStopsRunner(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	id, err := p.StartInstance(context.Background(), "daemon-exit", "")
	if err != nil {
		t.Fatal(err)
	}
	var daemon string
	for id, o := range m.objects {
		if o.Config.Labels["runscaler.dind-role"] == "daemon" {
			daemon = id
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.WaitInstance(ctx, id) }()
	if _, err := m.ContainerStop(ctx, daemon, dockerclient.ContainerStopOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("terminated runner must be reported as stopped so controller releases capacity: %v", err)
	}
	m.mu.Lock()
	stopped := m.stopped[id]
	m.mu.Unlock()
	if !stopped {
		t.Fatal("runner survived daemon exit")
	}
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestDinDMissingDaemonCompletesRepeatedObservation(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	id, err := p.StartInstance(context.Background(), "daemon-removed", "")
	if err != nil {
		t.Fatal(err)
	}
	for daemonID, o := range m.objects {
		if o.Config.Labels[dindRoleLabel] == "daemon" {
			delete(m.objects, daemonID)
		}
	}
	for range 2 {
		if err := p.WaitInstance(context.Background(), id); err != nil {
			t.Fatalf("missing daemon prevents controller eviction: %v", err)
		}
	}
	if !m.stopped[id] {
		t.Fatal("runner survived missing daemon")
	}
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestDinDObservationFailureDoesNotStopRunner(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	id, err := p.StartInstance(context.Background(), "monitor-retry", "")
	if err != nil {
		t.Fatal(err)
	}
	m.fail = "wait:daemon"
	if err := p.WaitInstance(context.Background(), id); err == nil {
		t.Fatal("monitor failure must request a retry")
	}
	m.mu.Lock()
	stopped := m.stopped[id]
	m.mu.Unlock()
	if stopped {
		t.Fatal("transient observation failure stopped a healthy runner")
	}
	m.mu.Lock()
	m.fail = ""
	m.mu.Unlock()
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestDinDConcurrentStartsDoNotShareState(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.StartInstance(context.Background(), fmt.Sprintf("parallel-%d", i), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	groups, err := FindDinDOrphans(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 4 {
		t.Fatalf("concurrent groups=%v", groups)
	}
	for _, g := range groups {
		if err := RemoveDinDInstance(context.Background(), m, g, slog.Default()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDinDSidecarDoesNotSeedPersistentCachesFromDaemonImage(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	if _, err := p.StartInstance(context.Background(), "cache-copy", ""); err != nil {
		t.Fatal(err)
	}
	for _, o := range m.objects {
		if o.Config.Labels[dindRoleLabel] != "daemon" {
			continue
		}
		for _, v := range o.HostConfig.Mounts {
			if v.Source == "persistent-shared" || v.Source == "persistent-cache" {
				if v.VolumeOptions == nil || !v.VolumeOptions.NoCopy {
					t.Fatalf("daemon image may populate runner cache %s", v.Source)
				}
			}
		}
	}
}

func TestDinDCanceledRemovalPreservesResources(t *testing.T) {
	m := newDinDFake()
	p := newDinDTestProvider(m)
	id, err := p.StartInstance(context.Background(), "canceled-remove", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.RemoveInstance(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup = %v", err)
	}
	if len(m.objects) != 2 || len(m.stores) != 5 {
		t.Fatal("canceled cleanup modified resources")
	}
	if err := p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

// Moby's wait API sends decoded results on an unbuffered channel. Cancellation
// must consume either that result or its context error rather than strand the
// SDK's response goroutine when a result races with cancellation.
type unbufferedWaitDocker struct {
	*mockDocker
	ready   chan struct{}
	sent    chan struct{}
	release chan struct{}
}

func (m *unbufferedWaitDocker) ContainerWait(ctx context.Context, _ string, _ dockerclient.ContainerWaitOptions) dockerclient.ContainerWaitResult {
	result := make(chan container.WaitResponse)
	err := make(chan error, 1)
	close(m.ready)
	go func() {
		<-ctx.Done()
		select {
		case result <- container.WaitResponse{}:
			close(m.sent)
		case <-m.release:
		}
	}()
	return dockerclient.ContainerWaitResult{Result: result, Error: err}
}

func TestDockerWaitCancellationConsumesDecodedResult(t *testing.T) {
	for _, initializer := range []bool{false, true} {
		t.Run(fmt.Sprintf("initializer=%v", initializer), func(t *testing.T) {
			m := &unbufferedWaitDocker{mockDocker: &mockDocker{imagePresent: true}, ready: make(chan struct{}), sent: make(chan struct{}), release: make(chan struct{})}
			t.Cleanup(func() { close(m.release) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			p, _ := newTestDockerProvider("", false)
			p.dockerClient = m
			go func() {
				if initializer {
					done <- waitDinDInitializer(ctx, m, "runner")
				} else {
					done <- p.waitContainer(ctx, "runner")
				}
			}()
			<-m.ready
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation result = %v", err)
			}
			select {
			case <-m.sent:
			case <-time.After(100 * time.Millisecond):
				t.Fatal("decoded SDK wait result was abandoned on cancellation")
			}
		})
	}
}
