package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ysya/runscaler/internal/backoff"
	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// AcquireTartInspectionLock serializes temporary guest inspection and orphan
// cleanup in one Tart home. It is separate from the manager's lifetime lock,
// so validate can inspect images while a manager runs. The persistent lock
// file must not be unlinked: all processes must contend on the same inode.
func AcquireTartInspectionLock(ctx context.Context, home string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	home, err := TartHomeDir(home)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("prepare Tart inspection lock: %w", err)
	}
	path := filepath.Join(home, ".runscaler-inspection.lock")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release, err := runnerlock.Acquire(path, runnerlock.Info{PID: os.Getpid(), StartedAt: time.Now()})
		if err == nil {
			if err := ctx.Err(); err != nil {
				release()
				return nil, err
			}
			return release, nil
		}
		if !errors.Is(err, runnerlock.ErrAlreadyRunning) {
			return nil, fmt.Errorf("acquire Tart inspection lock: %w", err)
		}
		if err := backoff.Sleep(ctx, 25*time.Millisecond); err != nil {
			return nil, err
		}
	}
}
