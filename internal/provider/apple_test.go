package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type appleFake struct {
	t          *testing.T
	items      []AppleInstance
	calls      [][]string
	jitPath    string
	failStart  bool
	failDelete bool
	badList    bool
	logs       string
}

func appleItem(id, owner, state string) AppleInstance {
	var item AppleInstance
	item.Configuration.ID = id
	item.Configuration.Labels = map[string]string{"io.github.runscaler.apple-owner": owner}
	item.Status.State = state
	return item
}
func (f *appleFake) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{}, args...))
	switch args[0] {
	case "list":
		if f.badList {
			return []byte("null"), nil
		}
		if f.items == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(f.items)
	case "run":
		name := ""
		for i, arg := range args {
			if strings.Contains(arg, "synthetic-jit") {
				f.t.Fatal("JIT leaked into command argv")
			}
			if arg == "--name" {
				name = args[i+1]
			}
			if arg == "--env-file" {
				f.jitPath = args[i+1]
				info, err := os.Stat(f.jitPath)
				if err != nil || info.Mode().Perm() != 0600 {
					f.t.Fatal("insecure JIT file")
				}
				data, err := os.ReadFile(f.jitPath)
				if err != nil || string(data) != "ACTIONS_RUNNER_INPUT_JITCONFIG=synthetic-jit\n" {
					f.t.Fatal("wrong JIT handoff")
				}
			}
		}
		f.items = append(f.items, appleItem(name, "example-set", "stopped"))
		if f.failStart {
			return nil, errors.New("ambiguous launch failure")
		}
		return []byte(name), nil
	case "stop":
		for i, item := range f.items {
			if item.Configuration.ID == args[len(args)-1] {
				f.items[i].Status.State = "stopped"
			}
		}
		return nil, nil
	case "delete":
		if f.failDelete {
			return nil, errors.New("cleanup failed")
		}
		for i, item := range f.items {
			if item.Configuration.ID == args[len(args)-1] {
				f.items = append(f.items[:i], f.items[i+1:]...)
				break
			}
		}
		return nil, nil
	case "logs":
		return []byte(f.logs), nil
	}
	return nil, errors.New("unexpected command")
}
func (f *appleFake) RunStreaming(context.Context, string, ...string) error {
	return errors.New("unexpected streaming")
}
func appleProvider(t *testing.T) (*AppleContainerProvider, *appleFake) {
	t.Helper()
	f := &appleFake{t: t}
	return &AppleContainerProvider{command: f, image: "example.invalid/runner@sha256:example", owner: "example-set", cpu: 2, memory: 1024, poll: time.Millisecond, readyTimeout: 10 * time.Millisecond}, f
}
func TestAppleLifecycleAndForeignPreservation(t *testing.T) {
	p, f := appleProvider(t)
	f.items = []AppleInstance{appleItem("foreign", "another-set", "running")}
	id, err := p.StartInstance(context.Background(), "example-runner", "synthetic-jit")
	if err != nil || id != "example-runner" {
		t.Fatal(id, err)
	}
	if _, err = os.Stat(f.jitPath); !os.IsNotExist(err) {
		t.Fatal("JIT file retained")
	}
	f.items[1].Status.State = "stopped"
	f.logs = "Listening for Jobs"
	if err = p.WaitReady(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err = p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err = p.RemoveInstance(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(f.items) != 1 || f.items[0].Configuration.ID != "foreign" {
		t.Fatal(f.items)
	}
	if err = p.RemoveInstance(context.Background(), "foreign"); err == nil {
		t.Fatal("foreign deletion accepted")
	}
}
func TestAppleAmbiguousLaunchCleanupAndReplay(t *testing.T) {
	p, f := appleProvider(t)
	f.failStart = true
	f.failDelete = true
	if _, err := p.StartInstance(context.Background(), "example-runner", "synthetic-jit"); err == nil {
		t.Fatal("launch failure hidden")
	}
	if len(f.items) != 1 {
		t.Fatal("failed owned resource lost")
	}
	if _, err := os.Stat(f.jitPath); !os.IsNotExist(err) {
		t.Fatal("JIT file retained")
	}
	f.failDelete = false
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.items) != 0 {
		t.Fatal("owned orphan retained")
	}
	f.failStart = false
	if _, err := p.StartInstance(context.Background(), "example-runner", "synthetic-jit"); err != nil {
		t.Fatal(err)
	}
}
func TestAppleCollisionMalformedEvidenceAndTimeout(t *testing.T) {
	p, f := appleProvider(t)
	f.items = []AppleInstance{appleItem("example-runner", "another-set", "running")}
	if _, err := p.StartInstance(context.Background(), "example-runner", "synthetic-jit"); err == nil {
		t.Fatal("foreign name reused")
	}
	f.badList = true
	if err := p.Reconcile(context.Background()); err == nil {
		t.Fatal("null inventory accepted")
	}
	f.badList = false
	if err := p.WaitReady(context.Background(), "example-runner"); !errors.Is(err, ErrReadinessTimeout) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.WaitInstance(ctx, "example-runner"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.items[0].Status.State = "stopped"
	if err := p.WaitInstance(context.Background(), "example-runner"); err != nil {
		t.Fatal(err)
	}
}
