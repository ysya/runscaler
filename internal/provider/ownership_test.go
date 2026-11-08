package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOwnsTartVMAcceptsOnlyRunnerGeneratedLocalNames(t *testing.T) {
	for _, name := range []string{NewRunnerName(), poolVMName(1, time.UnixMilli(1778066579017)), KeepVMName()} {
		if !OwnsTartVM(TartVM{Name: name, Source: "local"}) {
			t.Errorf("generated name %q not recognised", name)
		}
		if OwnsTartVM(TartVM{Name: name, Source: "OCI"}) {
			t.Errorf("OCI entry %q claimed", name)
		}
	}
	for _, name := range []string{"runner-nothex00", "pool-x-1778066579017", "runner-keep-golden", "macos-tahoe-base", "runner-deadbeef-2", "ghcr.io/cirruslabs/macos-tahoe-xcode:latest"} {
		if OwnsTartVM(TartVM{Name: name, Source: "local"}) {
			t.Errorf("look-alike %q claimed", name)
		}
	}
}

func TestOwnsContainerByLabelOrGeneratedName(t *testing.T) {
	if !OwnsContainer(map[string]string{ManagedByLabel: ManagedByValue}, nil) {
		t.Error("labelled container not recognised")
	}
	if !OwnsContainer(nil, []string{"/" + NewRunnerName()}) {
		t.Error("generated container name with Docker's leading slash not recognised")
	}
	if OwnsContainer(map[string]string{ManagedByLabel: "other"}, []string{"/runner-nothex00", "/pool-0-1"}) {
		t.Error("foreign container claimed")
	}
}

type listRunner struct {
	out   string
	err   error
	calls []string
}

func (r *listRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return []byte(r.out), r.err
}

func (r *listRunner) RunStreaming(ctx context.Context, name string, args ...string) error {
	_, err := r.Run(ctx, name, args...)
	return err
}

func TestListTartVMsDecodesNameSourceAndState(t *testing.T) {
	r := &listRunner{out: `[{"Name":"runner-deadbeef","Source":"local","State":"running"},{"Name":"ghcr.io/x:y","Source":"OCI","State":"stopped"}]`}
	vms, err := ListTartVMs(context.Background(), r)
	if err != nil || len(vms) != 2 || vms[0] != (TartVM{Name: "runner-deadbeef", Source: "local", State: "running"}) {
		t.Fatalf("vms=%+v err=%v", vms, err)
	}
	if len(r.calls) != 1 || r.calls[0] != "tart list --format json" {
		t.Fatalf("calls=%v", r.calls)
	}
	if _, err := ListTartVMs(context.Background(), &listRunner{out: "not json"}); err == nil {
		t.Fatal("invalid listing accepted")
	}
	if _, err := ListTartVMs(context.Background(), &listRunner{err: errors.New("tart missing")}); err == nil {
		t.Fatal("runner error swallowed")
	}
}

func TestTartCommandRunnerPassesHomeToChildren(t *testing.T) {
	out, err := TartCommandRunner("/tmp/tart home").Run(context.Background(), "sh", "-c", `printf %s "$TART_HOME"`)
	if err != nil || string(out) != "/tmp/tart home" {
		t.Fatalf("TART_HOME = %q, %v", out, err)
	}
	t.Setenv("TART_HOME", "/ambient/tart")
	out, err = TartCommandRunner("").Run(context.Background(), "sh", "-c", `printf %s "$TART_HOME"`)
	if err != nil || string(out) != "/ambient/tart" {
		t.Fatalf("an empty home must leave the ambient TART_HOME untouched: %q, %v", out, err)
	}
}
