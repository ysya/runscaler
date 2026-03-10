package config

import (
	"testing"

	"github.com/actions/scaleset"
)

func validScaleSetConfig() ScaleSetConfig {
	return ScaleSetConfig{
		RegistrationURL: "https://github.com/test-org",
		ScaleSetName:    "test-runners",
		Token:           "ghp_test",
		MaxRunners:      10,
		MinRunners:      0,
	}
}

func TestScaleSetConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*ScaleSetConfig)
		wantErr string
	}{
		{
			name:   "valid config",
			modify: func(c *ScaleSetConfig) {},
		},
		{
			name:    "missing url",
			modify:  func(c *ScaleSetConfig) { c.RegistrationURL = "" },
			wantErr: "registration URL",
		},
		{
			name:    "invalid url",
			modify:  func(c *ScaleSetConfig) { c.RegistrationURL = "not-a-url" },
			wantErr: "invalid registration URL",
		},
		{
			name:    "missing name",
			modify:  func(c *ScaleSetConfig) { c.ScaleSetName = "" },
			wantErr: "scale set name",
		},
		{
			name:    "missing token",
			modify:  func(c *ScaleSetConfig) { c.Token = "" },
			wantErr: "token",
		},
		{
			name:    "negative min runners",
			modify:  func(c *ScaleSetConfig) { c.MinRunners = -1 },
			wantErr: "min-runners must be >= 0",
		},
		{
			name:    "zero max runners",
			modify:  func(c *ScaleSetConfig) { c.MaxRunners = 0 },
			wantErr: "max-runners must be >= 1",
		},
		{
			name: "min exceeds max",
			modify: func(c *ScaleSetConfig) {
				c.MinRunners = 5
				c.MaxRunners = 3
			},
			wantErr: "min-runners (5) must be <= max-runners (3)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validScaleSetConfig()
			tt.modify(&c)
			err := c.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("Validate() expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestBuildLabels(t *testing.T) {
	t.Run("custom labels", func(t *testing.T) {
		c := ScaleSetConfig{
			ScaleSetName: "my-runners",
			Labels:       []string{"linux", "x64", "docker"},
		}
		labels := c.BuildLabels()

		if len(labels) != 3 {
			t.Fatalf("BuildLabels() got %d labels, want 3", len(labels))
		}
		want := []string{"linux", "x64", "docker"}
		for i, l := range labels {
			if l.Name != want[i] {
				t.Errorf("label[%d].Name = %q, want %q", i, l.Name, want[i])
			}
			if l.Type != "User" {
				t.Errorf("label[%d].Type = %q, want %q", i, l.Type, "User")
			}
		}
	})

	t.Run("defaults to scale set name", func(t *testing.T) {
		c := ScaleSetConfig{
			ScaleSetName: "my-runners",
			Labels:       nil,
		}
		labels := c.BuildLabels()

		if len(labels) != 1 {
			t.Fatalf("BuildLabels() got %d labels, want 1", len(labels))
		}
		if labels[0].Name != "my-runners" {
			t.Errorf("label[0].Name = %q, want %q", labels[0].Name, "my-runners")
		}
	})
}

func TestResolveScaleSets_Legacy(t *testing.T) {
	c := Config{
		RegistrationURL: "https://github.com/test-org",
		ScaleSetName:    "my-runners",
		Token:           "ghp_test",
		MaxRunners:      10,
		RunnerImage:     "ghcr.io/actions/actions-runner:latest",
		SharedVolume:    "/shared",
		DockerSocket:    "/var/run/docker.sock",
		DinD:            true,
	}

	sets := c.ResolveScaleSets()
	if len(sets) != 1 {
		t.Fatalf("expected 1 scale set, got %d", len(sets))
	}
	if sets[0].ScaleSetName != "my-runners" {
		t.Errorf("name = %q, want %q", sets[0].ScaleSetName, "my-runners")
	}
	if sets[0].MaxRunners != 10 {
		t.Errorf("max-runners = %d, want 10", sets[0].MaxRunners)
	}
	if sets[0].SharedVolume != "/shared" {
		t.Errorf("shared-volume = %q, want %q", sets[0].SharedVolume, "/shared")
	}
	if sets[0].DockerSocket != "/var/run/docker.sock" {
		t.Errorf("docker-socket = %q, want %q", sets[0].DockerSocket, "/var/run/docker.sock")
	}
	if !sets[0].IsDinD() {
		t.Error("IsDinD() = false, want true")
	}
}

func TestResolveScaleSets_Multi(t *testing.T) {
	dindFalse := false
	c := Config{
		RunnerImage:  "default-image:latest",
		RunnerGroup:  "default",
		MaxRunners:   5,
		SharedVolume: "/shared",
		DockerSocket: "/var/run/docker.sock",
		DinD:         true,
		ScaleSets: []ScaleSetConfig{
			{
				RegistrationURL: "https://github.com/org-a",
				ScaleSetName:    "runners-a",
				Token:           "token-a",
			},
			{
				RegistrationURL: "https://github.com/org-b",
				ScaleSetName:    "runners-b",
				Token:           "token-b",
				RunnerImage:     "custom-image:latest",
				MaxRunners:      20,
				SharedVolume:    "/data",
				DockerSocket:    "/run/podman/podman.sock",
				DinD:            &dindFalse,
			},
		},
	}

	sets := c.ResolveScaleSets()
	if len(sets) != 2 {
		t.Fatalf("expected 2 scale sets, got %d", len(sets))
	}

	// First should inherit all defaults
	if sets[0].RunnerImage != "default-image:latest" {
		t.Errorf("sets[0].RunnerImage = %q, want default", sets[0].RunnerImage)
	}
	if sets[0].MaxRunners != 5 {
		t.Errorf("sets[0].MaxRunners = %d, want 5 (inherited)", sets[0].MaxRunners)
	}
	if sets[0].RunnerGroup != "default" {
		t.Errorf("sets[0].RunnerGroup = %q, want default (inherited)", sets[0].RunnerGroup)
	}
	if sets[0].SharedVolume != "/shared" {
		t.Errorf("sets[0].SharedVolume = %q, want /shared (inherited)", sets[0].SharedVolume)
	}
	if sets[0].DockerSocket != "/var/run/docker.sock" {
		t.Errorf("sets[0].DockerSocket = %q, want /var/run/docker.sock (inherited)", sets[0].DockerSocket)
	}
	if !sets[0].IsDinD() {
		t.Error("sets[0].IsDinD() = false, want true (inherited)")
	}

	// Second should keep its own values
	if sets[1].RunnerImage != "custom-image:latest" {
		t.Errorf("sets[1].RunnerImage = %q, want custom", sets[1].RunnerImage)
	}
	if sets[1].MaxRunners != 20 {
		t.Errorf("sets[1].MaxRunners = %d, want 20", sets[1].MaxRunners)
	}
	if sets[1].SharedVolume != "/data" {
		t.Errorf("sets[1].SharedVolume = %q, want /data (per-scaleset override)", sets[1].SharedVolume)
	}
	if sets[1].DockerSocket != "/run/podman/podman.sock" {
		t.Errorf("sets[1].DockerSocket = %q, want podman socket (per-scaleset override)", sets[1].DockerSocket)
	}
	if sets[1].IsDinD() {
		t.Error("sets[1].IsDinD() = true, want false (per-scaleset override)")
	}
}

// --- Tart backend validation tests ---

func TestScaleSetConfigValidate_TartBackend(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*ScaleSetConfig)
		wantErr string
	}{
		{
			name: "valid tart config",
			modify: func(c *ScaleSetConfig) {
				c.Backend = "tart"
				c.TartImage = "macos-base:latest"
			},
		},
		{
			name: "tart missing image",
			modify: func(c *ScaleSetConfig) {
				c.Backend = "tart"
			},
			wantErr: "tart-image is required",
		},
		{
			name: "unsupported backend",
			modify: func(c *ScaleSetConfig) {
				c.Backend = "podman"
			},
			wantErr: "unsupported backend",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validScaleSetConfig()
			tt.modify(&c)
			err := c.Validate()

			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("Validate() expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestTartDefaults(t *testing.T) {
	c := Config{
		RegistrationURL: "https://github.com/test-org",
		ScaleSetName:    "macos-runners",
		Token:           "ghp_test",
		MaxRunners:      2,
		Backend:         "tart",
		TartImage:       "macos-base:latest",
	}

	sets := c.ResolveScaleSets()
	if len(sets) != 1 {
		t.Fatalf("expected 1 scale set, got %d", len(sets))
	}

	ss := sets[0]
	if ss.TartSSHUser != "admin" {
		t.Errorf("TartSSHUser = %q, want %q", ss.TartSSHUser, "admin")
	}
	if ss.TartSSHPass != "admin" {
		t.Errorf("TartSSHPass = %q, want %q", ss.TartSSHPass, "admin")
	}
	if ss.TartRunnerDir != "/Users/admin/actions-runner" {
		t.Errorf("TartRunnerDir = %q, want %q", ss.TartRunnerDir, "/Users/admin/actions-runner")
	}
	if !ss.IsTart() {
		t.Error("IsTart() = false, want true")
	}
}

func TestResolveScaleSets_MixedBackends(t *testing.T) {
	c := Config{
		MaxRunners: 5,
		TartImage:  "global-macos:latest",
		ScaleSets: []ScaleSetConfig{
			{
				RegistrationURL: "https://github.com/org",
				ScaleSetName:    "linux-runners",
				Token:           "token-a",
			},
			{
				RegistrationURL: "https://github.com/org",
				ScaleSetName:    "macos-runners",
				Token:           "token-b",
				Backend:         "tart",
				TartImage:       "custom-macos:latest",
			},
		},
	}

	sets := c.ResolveScaleSets()
	if len(sets) != 2 {
		t.Fatalf("expected 2 scale sets, got %d", len(sets))
	}

	// First: Docker (default)
	if sets[0].IsTart() {
		t.Error("sets[0] should not be tart")
	}

	// Second: Tart with custom image
	if !sets[1].IsTart() {
		t.Error("sets[1] should be tart")
	}
	if sets[1].TartImage != "custom-macos:latest" {
		t.Errorf("sets[1].TartImage = %q, want custom", sets[1].TartImage)
	}
	// Should have defaults applied
	if sets[1].TartSSHUser != "admin" {
		t.Errorf("sets[1].TartSSHUser = %q, want admin (default)", sets[1].TartSSHUser)
	}
}

func TestSystemInfo(t *testing.T) {
	info := SystemInfo(42)
	if info.ScaleSetID != 42 {
		t.Errorf("ScaleSetID = %d, want 42", info.ScaleSetID)
	}
	if info.System != "dockerscaleset" {
		t.Errorf("System = %q, want %q", info.System, "dockerscaleset")
	}
	if info == (scaleset.SystemInfo{}) {
		t.Error("SystemInfo() returned zero value")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
