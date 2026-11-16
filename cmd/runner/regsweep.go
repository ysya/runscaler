package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/regsweep"
)

// registrationSweepHTTPTimeout bounds each REST request of a sweep; the
// sweep as a whole is bounded by regsweep.DefaultTimeout.
const registrationSweepHTTPTimeout = 30 * time.Second

// newRegistrationSweeper builds the leaked-registration sweeper of one scale
// set, or returns nil when sweep-mode is off. With the mode off it reads no
// credential and makes no request. local must report the controller's
// instances; it is called only from the sweeper's own goroutine, which the
// caller starts after the controller exists.
func newRegistrationSweeper(ss config.ScaleSetConfig, api regsweep.ScaleSetAPI, groups scaleSetAPI, scaleSetID int, local regsweep.LocalRunners, logger *slog.Logger) (*regsweep.Sweeper, error) {
	mode := ss.EffectiveSweepMode()
	if mode == config.SweepModeOff {
		return nil, nil
	}
	org, ok := ss.SweepOrganization()
	if !ok {
		return nil, fmt.Errorf("sweep-mode %q requires an organization registration URL", mode)
	}
	base, err := regsweep.APIBaseURL(ss.RegistrationURL)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: registrationSweepHTTPTimeout}
	var tokens regsweep.TokenSource
	if ss.GitHubApp != nil {
		key, err := ss.GitHubAppPrivateKeyPEM()
		if err != nil {
			return nil, err
		}
		tokens, err = regsweep.NewAppTokenSource(ss.GitHubApp.ClientID, ss.GitHubApp.InstallationID, key, base, httpClient)
		if err != nil {
			return nil, err
		}
	} else {
		tokens = regsweep.StaticToken(ss.Token)
	}
	resolve := func(ctx context.Context) (int, error) {
		return resolveRunnerGroupID(ctx, groups, ss.RunnerGroup)
	}
	lister := regsweep.NewGroupLister(base, org, resolve, tokens, httpClient)
	sweeper, err := regsweep.New(regsweep.Config{
		Mode:       regsweep.Mode(mode),
		ScaleSetID: scaleSetID,
		Interval:   ss.EffectiveSweepInterval(),
	}, lister, api, local, logger)
	if err != nil {
		return nil, err
	}
	logger.Info("Registration sweep enabled",
		slog.String("mode", mode),
		slog.Duration("interval", ss.EffectiveSweepInterval()),
		slog.String("assumes", "one runner process per scale set"))
	return sweeper, nil
}

// startRegistrationSweep runs sweeper until the returned stop function is
// called; stop waits for a sweep in progress to end.
func startRegistrationSweep(ctx context.Context, sweeper *regsweep.Sweeper) (stop func()) {
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweeper.Run(sweepCtx)
	}()
	return func() {
		cancel()
		<-done
	}
}
