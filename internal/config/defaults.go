package config

// Default values for configuration fields.
// All flag definitions, config templates, and fallback logic
// should reference these constants instead of hardcoding values.
const (
	DefaultBackend       = "docker"
	DefaultMaxRunners    = 10
	DefaultRunnerImage   = "ghcr.io/actions/actions-runner:latest"
	DefaultRunnerGroup   = "default"
	DefaultDockerSocket  = "/var/run/docker.sock"
	DefaultDinD          = true
	DefaultTartRunnerDir = "/Users/admin/actions-runner"
	DefaultLogLevel      = "info"
	DefaultLogFormat     = "text"
	DefaultHealthPort    = 8080
	DefaultSystemName    = "dockerscaleset"
)
