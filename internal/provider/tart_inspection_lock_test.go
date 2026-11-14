package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTartInspectionLockFailureDoesNotStartClone(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(home, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &cloningRunner{home: home}
	if _, err := DetectTartGuest(context.Background(), runner, home, "fixture-image"); err == nil {
		t.Fatal("inspection ignored its lock failure")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("inspection ran commands without its lock: %v", runner.calls)
	}
}

func TestTartInspectionLockWaitIsCancellableAndHomesAreIndependent(t *testing.T) {
	home := t.TempDir()
	release, err := AcquireTartInspectionLock(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	runner := &cloningRunner{home: home, config: `{"os":"linux"}`}
	if _, err := DetectTartGuest(ctx, runner, home, "fixture-image"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked detection = %v, want deadline exceeded", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("cancelled inspection started Tart commands: %v", runner.calls)
	}
	otherCtx, otherCancel := context.WithTimeout(context.Background(), time.Second)
	defer otherCancel()
	otherRelease, err := AcquireTartInspectionLock(otherCtx, t.TempDir())
	if err != nil {
		t.Fatalf("an independent Tart home was blocked: %v", err)
	}
	otherRelease()
}

func TestTartInspectionLockReleasedAfterDetectionFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner cloningRunner
	}{
		{name: "clone failure", runner: cloningRunner{cloneErr: errors.New("clone failed")}},
		{name: "config failure", runner: cloningRunner{config: "invalid"}},
		{name: "partial clone failure", runner: cloningRunner{config: `{"os":"linux"}`, afterCloneErr: errors.New("prune failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.runner.home = home
			if _, err := DetectTartGuest(context.Background(), &tc.runner, home, "fixture-image"); err == nil {
				t.Fatal("fixture detection should fail")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err := AcquireTartInspectionLock(ctx, home)
			if err != nil {
				t.Fatalf("failed inspection retained its lock: %v", err)
			}
			release()
		})
	}
}

type lockedDeletionRunner struct {
	*cloningRunner
	t *testing.T
}

func (r lockedDeletionRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if args[0] == "delete" {
		waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		release, err := AcquireTartInspectionLock(waitCtx, r.home)
		if err == nil {
			release()
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			r.t.Errorf("inspection released its lock before deferred deletion: %v", err)
		}
	}
	return r.cloningRunner.Run(ctx, name, args...)
}

func TestTartInspectionLockHeldThroughCancelledCloneDeletion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	home := t.TempDir()
	runner := lockedDeletionRunner{cloningRunner: &cloningRunner{home: home, config: `{"os":"linux"}`, onClone: cancel}, t: t}
	if _, err := DetectTartGuest(ctx, runner, home, "fixture-image"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled detection = %v", err)
	}
	if runner.deleteCtxErr != nil {
		t.Fatalf("deferred deletion used a cancelled context: %v", runner.deleteCtxErr)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Second)
	defer checkCancel()
	release, err := AcquireTartInspectionLock(checkCtx, home)
	if err != nil {
		t.Fatalf("cancelled detection retained its lock: %v", err)
	}
	release()
}

func TestTartInspectionLockProcessHelper(t *testing.T) {
	home := os.Getenv("RUNNER_TART_INSPECTION_LOCK_HELPER")
	if home == "" {
		t.Skip("subprocess fixture only")
	}
	if _, err := AcquireTartInspectionLock(context.Background(), home); err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	os.Exit(0) // Deliberately bypass release: the kernel must unlock on exit.
}

func TestTartInspectionLockReleasedWhenAnotherProcessExits(t *testing.T) {
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTartInspectionLockProcessHelper$")
	cmd.Env = append(os.Environ(), "RUNNER_TART_INSPECTION_LOCK_HELPER="+home)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("child did not acquire lock: %q, %v", line, err)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer waitCancel()
	if release, err := AcquireTartInspectionLock(waitCtx, home); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("lock held by another process = %v", err)
	}
	if _, err := in.Write([]byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireTartInspectionLock(ctx, home)
	if err != nil {
		t.Fatalf("exited process retained the lock: %v", err)
	}
	release()
}
