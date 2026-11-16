package regsweep

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/controller"
)

// bootingProvider holds every StartInstance until release closes, so the
// runner stays in the controller's provisioning phase while GitHub already
// lists its registration offline.
type bootingProvider struct {
	started chan string
	release chan struct{}
}

func (p *bootingProvider) StartInstance(ctx context.Context, name, _ string) (string, error) {
	p.started <- name
	select {
	case <-p.release:
		return "", errors.New("boot failed")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func (p *bootingProvider) RemoveInstance(context.Context, string) error { return nil }
func (p *bootingProvider) Shutdown(context.Context)                     {}

// jitScaleSet issues JIT registrations with increasing runner IDs and
// records them, standing in for both the controller's and the sweep's scale
// set API.
type jitScaleSet struct {
	fakeScaleSet
	next int
}

func (j *jitScaleSet) GenerateJitRunnerConfig(_ context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.next++
	if j.refs == nil {
		j.refs = map[int]*scaleset.RunnerReference{}
	}
	j.refs[j.next] = &scaleset.RunnerReference{ID: j.next, Name: setting.Name, RunnerScaleSetID: scaleSetID}
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: j.next}, EncodedJITConfig: "jit"}, nil
}

// A registration whose runner the controller is still provisioning (a VM
// booting) is kept; once the start fails and the controller drops the runner,
// the sweep that the failure triggers removes the leaked registration.
func TestSweepWithControllerKeepsProvisioningRunnerAndRemovesItAfterStartFailure(t *testing.T) {
	prov := &bootingProvider{started: make(chan string, 1), release: make(chan struct{})}
	api := &jitScaleSet{}
	gh := &fakeGitHub{t: t}
	e := newEnvWith(t, ModeRemove, gh, api)

	// Each notice reports whether the failed runner was still local then.
	notices := make(chan bool, 8)
	var ctl *controller.ScaleSetController
	ctlReady := make(chan struct{})
	ctl = controller.NewScaleSetController(testScaleSetID, 0, 1, prov, api, slog.New(slog.DiscardHandler),
		controller.WithStartFailureNotifier(func() {
			<-ctlReady
			select {
			case notices <- ctl.HasRunner(lastName(api)):
			default:
			}
		}))
	close(ctlReady)
	defer ctl.Shutdown(context.Background())
	e.sweeper.local = ctl

	if _, err := ctl.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	var name string
	select {
	case name = <-prov.started:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not start a runner")
	}
	gh.mu.Lock()
	gh.pages = [][]Runner{{{ID: 1, Name: name, Status: "offline"}}}
	gh.mu.Unlock()

	if !ctl.HasRunner(name) {
		t.Fatal("provisioning runner is not local")
	}
	res := e.sweeper.Sweep(context.Background())
	if res.Removed != 0 || len(api.removedIDs()) != 0 {
		t.Fatalf("sweep removed the registration of a provisioning runner: %+v", res)
	}

	close(prov.release) // the boot fails
	var stillLocal bool
	select {
	case stillLocal = <-notices:
	case <-time.After(5 * time.Second):
		t.Fatal("start failure sent no notice")
	}
	if stillLocal {
		t.Fatal("start-failure notice came before the runner left local state")
	}
	res = e.sweeper.Sweep(context.Background())
	if res.Removed != 1 {
		t.Fatalf("sweep after the start failure = %+v, want the leaked registration removed", res)
	}
}

func lastName(j *jitScaleSet) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ref := j.refs[j.next]; ref != nil {
		return ref.Name
	}
	return ""
}
