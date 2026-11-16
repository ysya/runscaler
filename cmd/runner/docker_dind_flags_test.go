package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestLoadConfigDockerDaemonFlagPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name, settings string
		args           []string
		mode, image    string
		timeout        time.Duration
		nilTimeout     bool
		memory, cpu    int
		pids           int64
	}{
		{name: "omitted timeout remains nil", settings: `dind = false`, mode: "none", image: "docker:29-dind", timeout: 120 * time.Second, nilTimeout: true},
		{name: "unchanged defaults preserve file", settings: `daemon-mode = "dind"
dind-image = "example:private"
dind-startup-timeout = "33s"
dind-memory = 2048
dind-cpu = 3
dind-pids-limit = 256`, mode: "dind", image: "example:private", timeout: 33 * time.Second, memory: 2048, cpu: 3, pids: 256},
		{name: "explicit flags win", settings: `daemon-mode = "none"
dind-startup-timeout = "33s"
dind-memory = 2048`, args: []string{"--docker-daemon-mode=dind", "--dind-image=example:cli", "--dind-startup-timeout=10s", "--dind-memory=0", "--dind-cpu=2", "--dind-pids-limit=32"}, mode: "dind", image: "example:cli", timeout: 10 * time.Second, cpu: 2, pids: 32},
		{name: "explicit empty mode uses legacy flag", settings: `daemon-mode = "dind"
dind = true`, args: []string{"--docker-daemon-mode=", "--dind=false"}, mode: "none", image: "docker:29-dind", timeout: 120 * time.Second, nilTimeout: true},
		{name: "explicit zero timeout is preserved for validation", settings: `daemon-mode = "dind"`, args: []string{"--dind-startup-timeout=0s"}, mode: "dind", image: "docker:29-dind"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)
			p := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(p, []byte("[docker]\n"+tt.settings+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			c := &cobra.Command{}
			f := c.Flags()
			f.String("config", p, "")
			f.Bool("dind", true, "")
			registerDockerDaemonFlags(f)
			if err := viper.BindPFlag("docker.dind", f.Lookup("dind")); err != nil {
				t.Fatal(err)
			}
			if err := f.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(c)
			if err != nil {
				t.Fatal(err)
			}
			dc := cfg.Defaults.Docker
			if got := cfg.Defaults.DockerDaemonMode(); got != tt.mode {
				t.Errorf("mode=%q,want %q", got, tt.mode)
			}
			if dc.EffectiveDinDImage() != tt.image || dc.EffectiveDinDStartupTimeout() != tt.timeout {
				t.Errorf("image/timeout = %s/%s", dc.EffectiveDinDImage(), dc.EffectiveDinDStartupTimeout())
			}
			if (dc.DinDStartupTimeout == nil) != tt.nilTimeout {
				t.Errorf("nil timeout=%v,want %v", dc.DinDStartupTimeout == nil, tt.nilTimeout)
			}
			if dc.DinDMemory != tt.memory || dc.DinDCPU != tt.cpu || dc.DinDPidsLimit != tt.pids {
				t.Errorf("resource limits=%d/%d/%d", dc.DinDMemory, dc.DinDCPU, dc.DinDPidsLimit)
			}
			if len(cfg.Warnings) > 0 {
				t.Errorf("warnings=%v", cfg.Warnings)
			}
		})
	}
}

func TestLoadConfigDockerDaemonFlagsAreScaleSetDefaults(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(`
[docker]
dind = false
[[scaleset]]
name = "inherits"
[[scaleset]]
name = "legacy-fallback"
[scaleset.docker]
daemon-mode = ""
`), 0600); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	f := c.Flags()
	f.String("config", p, "")
	registerDockerDaemonFlags(f)
	if err := f.Parse([]string{"--docker-daemon-mode=dind", "--dind-startup-timeout=15s"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	sets := cfg.ResolveScaleSets()
	if sets[0].DockerDaemonMode() != "dind" || sets[1].DockerDaemonMode() != "none" {
		t.Fatalf("incorrect mode inheritance: %s/%s", sets[0].DockerDaemonMode(), sets[1].DockerDaemonMode())
	}
	for i := range sets {
		if sets[i].Docker.EffectiveDinDStartupTimeout() != 15*time.Second {
			t.Errorf("set %d did not inherit CLI timeout", i)
		}
	}
}
