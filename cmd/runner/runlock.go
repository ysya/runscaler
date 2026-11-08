package main

import (
	"errors"
	"os"
	"time"

	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// runnerLockPath is the machine-wide lock `runner run` holds. Installation
// and doctor probe it; tests point it at a temporary file.
var runnerLockPath = runnerlock.DefaultPath

// probeRunningRunner reports the runner holding the machine-wide lock, or
// nil when none does. It holds the lock only for an instant, so whatever
// starts next, such as a service being installed, can take it.
func probeRunningRunner() (*runnerlock.AlreadyRunningError, error) {
	release, err := runnerlock.Acquire(runnerLockPath, runnerlock.Info{PID: os.Getpid(), StartedAt: time.Now()})
	if err == nil {
		release()
		return nil, nil
	}
	var running *runnerlock.AlreadyRunningError
	if errors.As(err, &running) {
		return running, nil
	}
	return nil, err
}
