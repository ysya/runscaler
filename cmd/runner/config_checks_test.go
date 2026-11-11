package main

import (
	"strings"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

func validScaleSet(name string, minRunners int) config.ScaleSetConfig {
	return config.ScaleSetConfig{
		RegistrationURL: "https://github.com/example",
		ScaleSetName:    name,
		Token:           "synthetic",
		RunnerImage:     config.DefaultRunnerImage,
		RunnerGroup:     config.DefaultRunnerGroup,
		MaxRunners:      5,
		MinRunners:      minRunners,
		Provider:        config.DefaultProvider,
		Docker:          config.DockerConfig{Socket: config.DefaultDockerSocket},
	}
}

// `runner validate` and service preparation must reject everything
// `runner run` rejects before starting, so a config that validates starts.
func TestCheckScaleSetsRejectsWhatRunRejects(t *testing.T) {
	if err := checkScaleSets(config.Config{}, []config.ScaleSetConfig{validScaleSet("a", 0)}); err != nil {
		t.Fatalf("a valid scale set = %v", err)
	}
	if err := checkScaleSets(config.Config{Concurrent: 1}, []config.ScaleSetConfig{validScaleSet("a", 1), validScaleSet("b", 1)}); err == nil || !strings.Contains(err.Error(), "concurrent") {
		t.Errorf("concurrent below the sum of min-runners = %v", err)
	}
	if err := checkScaleSets(config.Config{}, []config.ScaleSetConfig{validScaleSet("a", 0), validScaleSet("a", 0)}); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("duplicate names = %v", err)
	}
	bad := validScaleSet("a", 0)
	bad.RunnerImage = ""
	if err := checkScaleSets(config.Config{}, []config.ScaleSetConfig{bad}); err == nil || !strings.Contains(err.Error(), `scaleset[0] "a"`) {
		t.Errorf("an invalid scale set = %v, want it named", err)
	}
}

func TestCheckScaleSetsNativeActionsCacheRequiresMatchingReceiver(t *testing.T) {
	ss := validScaleSet("native", 0)
	ss.Docker.NativeActionsCache = true
	ss.Docker.ActionsCacheURL = "http://192.168.50.10:3001/"
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "disabled", want: "actions-cache.enabled"},
		{name: "mismatch", cfg: config.Config{ActionsCache: config.ActionsCacheConfig{Enabled: true, AdvertiseURL: "http://192.168.50.10:3002/"}}, want: "actions-cache.advertise-url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkScaleSets(tc.cfg, []config.ScaleSetConfig{ss}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkScaleSets() = %v, want %s error", err, tc.want)
			}
		})
	}
}
