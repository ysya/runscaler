//go:build darwin

package provider

import (
	"context"
	"github.com/ysya/runscaler/internal/config"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSeatbeltLifecycle(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(root, "template")
	state := filepath.Join(root, "jobs")
	os.MkdirAll(filepath.Join(template, "bin"), 0700)
	os.WriteFile(filepath.Join(template, "bin/Runner.Listener"), []byte("fixture"), 0600)
	os.WriteFile(filepath.Join(template, "run.sh"), []byte("#!/bin/sh\nsleep 120 &\necho $! > child.pid\necho 'Listening for Jobs'\nwait\n"), 0700)
	sdk := filepath.Join(root, "sdk")
	os.MkdirAll(sdk, 0700)
	os.WriteFile(filepath.Join(sdk, "source"), []byte("original"), 0600)
	p := NewSeatbeltProvider(config.ScaleSetConfig{Seatbelt: config.SeatbeltConfig{RunnerDir: template, StateDir: state, Path: "/usr/bin:/bin", Environment: map[string]string{"android_home": sdk, "android_sdk_root": sdk}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := p.StartInstance(ctx, "ignored", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(ctx)
	privateSDK := filepath.Join(id, "home", "android-sdk", "source")
	if err := os.WriteFile(privateSDK, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if original, err := os.ReadFile(filepath.Join(sdk, "source")); err != nil || string(original) != "original" {
		t.Fatalf("SDK clone changed source: %q %v", original, err)
	}
	if err = p.WaitReady(ctx, id); err != nil {
		data, _ := os.ReadFile(filepath.Join(id, "runner.log"))
		t.Fatalf("%v: %s exit=%v", err, data, p.instance(id).err)
	}
	if err := os.MkdirAll(filepath.Join(id, "readonly"), 0500); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(id, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	recovery := NewSeatbeltProvider(config.ScaleSetConfig{Seatbelt: p.cfg})
	if err = recovery.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(id); !os.IsNotExist(err) {
		t.Fatal("job directory remains")
	}
	until := time.Now().Add(time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		state, _ := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if !strings.Contains(string(state), "Z") {
			t.Fatalf("child survives cleanup: %s", state)
		}
	}
	if err = p.RemoveInstance(ctx, id); err != nil {
		t.Fatal(err)
	}
}
