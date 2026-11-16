package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/actions/scaleset"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/regsweep"
)

type noRunnerAPI struct{}

func (noRunnerAPI) GetRunner(context.Context, int) (*scaleset.RunnerReference, error) {
	return nil, scaleset.NotFoundError
}
func (noRunnerAPI) RemoveRunner(context.Context, int64) error { return nil }

func sweepScaleSet(mode string) config.ScaleSetConfig {
	return config.ScaleSetConfig{RegistrationURL: "https://github.com/example-org", ScaleSetName: "runners", Token: "ghp_test", SweepMode: mode}
}

var noLocal = regsweep.LocalRunnersFunc(func(string) bool { return false })

func TestRegistrationSweeperOffReadsNoCredential(t *testing.T) {
	ss := sweepScaleSet("")
	ss.Token = ""
	// An App key that cannot be read: with the sweep off it is never read.
	ss.GitHubApp = &config.GitHubAppConfig{ClientID: "Iv1.x", InstallationID: 1, PrivateKeyFile: "/nonexistent/key.pem"}
	sw, err := newRegistrationSweeper(ss, noRunnerAPI{}, nil, 5, noLocal, slog.New(slog.DiscardHandler))
	if sw != nil || err != nil {
		t.Fatalf("sweep off = %v, %v; want nil, nil", sw, err)
	}
}

func TestRegistrationSweeperOnBuildsForOrgScaleSet(t *testing.T) {
	sw, err := newRegistrationSweeper(sweepScaleSet(config.SweepModeLog), noRunnerAPI{}, nil, 5, noLocal, slog.New(slog.DiscardHandler))
	if sw == nil || err != nil {
		t.Fatalf("sweep log = %v, %v", sw, err)
	}
}

func TestRegistrationSweeperRefusesRepositoryScope(t *testing.T) {
	ss := sweepScaleSet(config.SweepModeRemove)
	ss.RegistrationURL = "https://github.com/example-org/repo"
	if _, err := newRegistrationSweeper(ss, noRunnerAPI{}, nil, 5, noLocal, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("repository scale set accepted")
	}
}

func TestRegistrationSweeperRefusesUnknownScaleSetID(t *testing.T) {
	if _, err := newRegistrationSweeper(sweepScaleSet(config.SweepModeRemove), noRunnerAPI{}, nil, 0, noLocal, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("scale set ID 0 accepted")
	}
}
