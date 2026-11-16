package main

import (
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

func registerDockerDaemonFlags(flags *pflag.FlagSet) {
	flags.String("docker-daemon-mode", "", "Docker daemon access: host (share outer socket), dind (per-job daemon), none (legacy --dind applies when empty)")
	flags.String("dind-image", config.DefaultDinDImage, "Docker image for each per-job Docker daemon")
	flags.Duration("dind-startup-timeout", config.DefaultDinDStartupTimeout, "Startup timeout for each per-job Docker daemon")
	flags.Int("dind-memory", 0, "Memory limit in MB for each per-job Docker daemon (0 = unlimited)")
	flags.Int("dind-cpu", 0, "CPU cores for each per-job Docker daemon (0 = unlimited)")
	flags.Int64("dind-pids-limit", 0, "PID limit for each per-job Docker daemon (0 = unlimited)")
}

func bindDockerDaemonFlags(v *viper.Viper, flags *pflag.FlagSet) {
	for _, name := range []string{"docker-daemon-mode", "dind-image", "dind-startup-timeout", "dind-memory", "dind-cpu", "dind-pids-limit"} {
		flag := flags.Lookup(name)
		if flag == nil {
			continue
		}
		// Keep omission distinct from an explicit zero timeout. A bound unchanged
		// duration default would otherwise manufacture a non-nil config pointer.
		if name == "dind-startup-timeout" && !flag.Changed {
			continue
		}
		key := name
		if name == "docker-daemon-mode" {
			key = "daemon-mode"
		}
		_ = v.BindPFlag("docker."+key, flag)
	}
}
