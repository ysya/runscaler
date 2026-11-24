package config

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultDinDImage          = "docker:29-dind"
	DefaultDinDStartupTimeout = 120 * time.Second
)

// DockerDaemonMode resolves the explicit mode before the legacy socket-sharing setting.
func (ss *ScaleSetConfig) DockerDaemonMode() string {
	if ss.Docker.DaemonMode != "" {
		return ss.Docker.DaemonMode
	}
	if ss.IsDinD() {
		return "host"
	}
	return "none"
}

func (dc DockerConfig) EffectiveDinDImage() string {
	if dc.DinDImage != "" {
		return dc.DinDImage
	}
	return DefaultDinDImage
}

func (dc DockerConfig) EffectiveDinDStartupTimeout() time.Duration {
	if dc.DinDStartupTimeout != nil {
		return *dc.DinDStartupTimeout
	}
	return DefaultDinDStartupTimeout
}

func (ss *ScaleSetConfig) validateDockerDaemon() error {
	switch ss.DockerDaemonMode() {
	case "host", "dind", "none":
	default:
		return fmt.Errorf("docker daemon-mode must be host, dind, or none")
	}
	dc := ss.Docker
	if dc.EffectiveDinDStartupTimeout() <= 0 {
		return fmt.Errorf("docker dind-startup-timeout must be > 0")
	}
	if dc.DinDMemory < 0 || dc.DinDCPU < 0 || dc.DinDPidsLimit < 0 {
		return fmt.Errorf("docker dind resource limits must be >= 0")
	}
	if ss.DockerDaemonMode() != "dind" {
		return nil
	}
	if dc.Network == "host" || strings.HasPrefix(dc.Network, "container:") {
		return fmt.Errorf("docker dind network must use a private namespace; host and container: modes are unsupported")
	}
	if dc.Platform != "" && !strings.HasPrefix(dc.Platform, "linux/") {
		return fmt.Errorf("docker dind requires a linux platform")
	}
	if p := dc.SharedVolume; p != "" {
		for _, reserved := range []string{"/var/run", "/run", "/var/lib/docker", "/home/runner/_work", "/home/runner/externals"} {
			if dockerPathsOverlap(p, reserved) {
				return fmt.Errorf("docker dind mount %q overlaps reserved path %q", p, reserved)
			}
		}
	}
	return nil
}

func dockerPathsOverlap(a, b string) bool {
	return a == b || a == "/" || b == "/" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
