package config

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestDockerDaemonMode(t *testing.T) {
	yes, no := true, false
	for _, tt := range []struct {
		mode   string
		legacy *bool
		want   string
	}{
		{"", nil, "host"}, {"", &yes, "host"}, {"", &no, "none"},
		{"host", &no, "host"}, {"dind", &no, "dind"}, {"none", &yes, "none"},
	} {
		ss := ScaleSetConfig{Docker: DockerConfig{DaemonMode: tt.mode, DinD: tt.legacy}}
		if got := ss.DockerDaemonMode(); got != tt.want {
			t.Errorf("mode %q legacy %v = %q, want %q", tt.mode, tt.legacy, got, tt.want)
		}
	}
}

func TestDinDDoesNotUseHostWorkspaceProxy(t *testing.T) {
	for _, mode := range []string{"dind", "none"} {
		ss := validScaleSetConfig()
		ss.Docker.DaemonMode = mode
		ss.Docker.WorkspaceRoot = "/srv/runner-workspaces"
		if err := ss.Validate(); err == nil {
			t.Fatalf("%s accepted a host socket proxy", mode)
		}
	}
	ss := validScaleSetConfig()
	no := false
	ss.Docker = DockerConfig{DaemonMode: "host", DinD: &no, WorkspaceRoot: "/srv/runner-workspaces"}
	if err := ss.Validate(); err != nil {
		t.Fatalf("explicit host mode did not override legacy false: %v", err)
	}
}

func TestDinDEffectiveDefaults(t *testing.T) {
	dc := DockerConfig{}
	if dc.EffectiveDinDImage() != "docker:29-dind" || dc.EffectiveDinDStartupTimeout() != 120*time.Second {
		t.Fatal("incorrect DinD defaults")
	}
	timeout := 31 * time.Second
	dc.DinDImage, dc.DinDStartupTimeout = "example:dind", &timeout
	if dc.EffectiveDinDImage() != "example:dind" || dc.EffectiveDinDStartupTimeout() != timeout {
		t.Fatal("explicit DinD settings ignored")
	}
}

func TestDinDValidation(t *testing.T) {
	zero, negative := time.Duration(0), -time.Second
	for _, tt := range []struct {
		name   string
		modify func(*DockerConfig)
		want   string
	}{
		{"mode", func(d *DockerConfig) { d.DaemonMode = "invalid" }, "daemon-mode"},
		{"zero timeout", func(d *DockerConfig) { d.DinDStartupTimeout = &zero }, "dind-startup-timeout"},
		{"negative timeout", func(d *DockerConfig) { d.DinDStartupTimeout = &negative }, "dind-startup-timeout"},
		{"memory", func(d *DockerConfig) { d.DinDMemory = -1 }, "resource"},
		{"cpu", func(d *DockerConfig) { d.DinDCPU = -1 }, "resource"},
		{"pids", func(d *DockerConfig) { d.DinDPidsLimit = -1 }, "resource"},
		{"windows", func(d *DockerConfig) { d.Platform = "windows/amd64" }, "linux"},
		{"darwin", func(d *DockerConfig) { d.Platform = "darwin/arm64" }, "linux"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ss := validScaleSetConfig()
			ss.Docker.DaemonMode = "dind"
			tt.modify(&ss.Docker)
			if err := ss.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want %q", err, tt.want)
			}
		})
	}
	for _, platform := range []string{"", "linux/amd64", "linux/arm64/v8"} {
		ss := validScaleSetConfig()
		ss.Docker.DaemonMode = "dind"
		ss.Docker.Platform = platform
		if err := ss.Validate(); err != nil {
			t.Errorf("platform %q: %v", platform, err)
		}
	}
}

func TestDinDReservedMountValidation(t *testing.T) {
	for _, reserved := range []string{"/var/run", "/run", "/var/lib/docker", "/home/runner/_work", "/home/runner/externals"} {
		for _, p := range []string{reserved, reserved + "/nested", reserved[:strings.LastIndex(reserved, "/")]} {
			if p == "" {
				p = "/"
			}
			for _, source := range []string{"shared", "cache"} {
				t.Run(source+p, func(t *testing.T) {
					ss := validScaleSetConfig()
					ss.Docker.DaemonMode = "dind"
					if source == "shared" {
						ss.Docker.SharedVolume = p
					} else {
						ss.Docker.CacheVolumes = []string{"cache:" + p}
					}
					if err := ss.Validate(); err == nil {
						t.Fatalf("accepted managed path overlap %q", p)
					}
				})
			}
		}
	}
	for _, tt := range []struct {
		shared string
		cache  []string
	}{
		{"/cache", []string{"one:/cache/nested"}}, {"/cache/nested", []string{"one:/cache"}},
		{"", []string{"one:/cache", "two:/cache/nested"}},
	} {
		ss := validScaleSetConfig()
		ss.Docker = DockerConfig{DaemonMode: "dind", SharedVolume: tt.shared, CacheVolumes: tt.cache}
		if err := ss.Validate(); err == nil {
			t.Fatalf("accepted overlapping shared/cache mounts %+v", tt)
		}
		ss.Docker.DaemonMode = "host"
		if err := ss.Validate(); err != nil {
			t.Fatalf("changed legacy mount semantics: %v", err)
		}
	}
	ss := validScaleSetConfig()
	ss.Docker = DockerConfig{DaemonMode: "dind", SharedVolume: "/shared", CacheVolumes: []string{"one:/cache", "two:/cache-other"}}
	if err := ss.Validate(); err != nil {
		t.Fatalf("distinct paths rejected: %v", err)
	}
}

func TestLoadDinDInheritance(t *testing.T) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(`
[docker]
dind = false
daemon-mode = "dind"
dind-image = "example:dind"
dind-startup-timeout = "45s"
dind-memory = 1024
dind-cpu = 2
dind-pids-limit = 512
[[scaleset]]
name = "inherits-mode"
[[scaleset]]
name = "legacy-empty"
[scaleset.docker]
daemon-mode = ""
[[scaleset]]
name = "explicit-host"
[scaleset.docker]
daemon-mode = "host"
dind-startup-timeout = "0s"
dind-memory = 0
`)); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("warnings: %v", cfg.Warnings)
	}
	sets := cfg.ResolveScaleSets()
	for i, want := range []string{"dind", "none", "host"} {
		if got := sets[i].DockerDaemonMode(); got != want {
			t.Errorf("set %d mode=%q, want %q", i, got, want)
		}
		if sets[i].Docker.DinDImage != "example:dind" || sets[i].Docker.DinDCPU != 2 || sets[i].Docker.DinDPidsLimit != 512 {
			t.Errorf("set %d failed to inherit", i)
		}
	}
	if sets[0].Docker.EffectiveDinDStartupTimeout() != 45*time.Second || sets[0].Docker.DinDMemory != 1024 {
		t.Fatal("DinD inheritance failed")
	}
	if sets[2].Docker.DinDStartupTimeout == nil || *sets[2].Docker.DinDStartupTimeout != 0 || sets[2].Docker.DinDMemory != 0 {
		t.Fatal("explicit zero lost during inheritance")
	}
}

func TestDinDRejectsSharedNetworkNamespaces(t *testing.T) {
	for _, network := range []string{"host", "container:another-instance"} {
		for _, mode := range []string{"dind", "host", "none"} {
			t.Run(mode+"/"+network, func(t *testing.T) {
				ss := validScaleSetConfig()
				ss.Docker = DockerConfig{DaemonMode: mode, Network: network}
				err := ss.Validate()
				if mode == "dind" {
					if err == nil || !strings.Contains(err.Error(), "network") {
						t.Fatalf("Validate()=%v, want network namespace rejection", err)
					}
				} else if err != nil {
					t.Fatalf("legacy network behavior changed: %v", err)
				}
			})
		}
	}
	for _, network := range []string{"", "bridge", "none", "dedicated-runners"} {
		t.Run(network, func(t *testing.T) {
			ss := validScaleSetConfig()
			ss.Docker = DockerConfig{DaemonMode: "dind", Network: network}
			if err := ss.Validate(); err != nil {
				t.Fatalf("valid network %q rejected: %v", network, err)
			}
		})
	}
}
