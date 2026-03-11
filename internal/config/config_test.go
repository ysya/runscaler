package config

import (
	"testing"
)

func validScaleSetConfig() ScaleSetConfig {
	return ScaleSetConfig{
		RegistrationURL: "https://github.com/test-org",
		ScaleSetName:    "test-runners",
		Token:           "ghp_test",
		MaxRunners:      10,
		MinRunners:      0,
		Backend:         DefaultBackend,
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
		labels := BuildLabels("my-runners", []string{"linux", "x64", "docker"})

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
		labels := BuildLabels("my-runners", nil)

		if len(labels) != 1 {
			t.Fatalf("BuildLabels() got %d labels, want 1", len(labels))
		}
		if labels[0].Name != "my-runners" {
			t.Errorf("label[0].Name = %q, want %q", labels[0].Name, "my-runners")
		}
	})
}

func TestResolveScaleSets_Legacy(t *testing.T) {
	dindTrue := true
	c := Config{
		Defaults: ScaleSetConfig{
			RegistrationURL: "https://github.com/test-org",
			ScaleSetName:    "my-runners",
			Token:           "ghp_test",
			MaxRunners:      10,
			RunnerImage:     DefaultRunnerImage,
			Docker: DockerConfig{
				SharedVolume: "/shared",
				Socket:       DefaultDockerSocket,
				DinD:         &dindTrue,
			},
		},
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
	if sets[0].Docker.SharedVolume != "/shared" {
		t.Errorf("shared-volume = %q, want %q", sets[0].Docker.SharedVolume, "/shared")
	}
	if sets[0].Docker.Socket != DefaultDockerSocket {
		t.Errorf("docker-socket = %q, want %q", sets[0].Docker.Socket, DefaultDockerSocket)
	}
	if !sets[0].IsDinD() {
		t.Error("IsDinD() = false, want true")
	}
	if sets[0].Backend != DefaultBackend {
		t.Errorf("Backend = %q, want %q", sets[0].Backend, DefaultBackend)
	}
}

func TestResolveScaleSets_Multi(t *testing.T) {
	dindTrue := true
	dindFalse := false
	c := Config{
		Defaults: ScaleSetConfig{
			RunnerImage: "default-image:latest",
			RunnerGroup: "default",
			MaxRunners:  5,
			Docker: DockerConfig{
				SharedVolume: "/shared",
				Socket:       DefaultDockerSocket,
				DinD:         &dindTrue,
			},
		},
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
				Docker: DockerConfig{
					SharedVolume: "/data",
					Socket:       "/run/podman/podman.sock",
					DinD:         &dindFalse,
				},
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
	if sets[0].Docker.SharedVolume != "/shared" {
		t.Errorf("sets[0].Docker.SharedVolume = %q, want /shared (inherited)", sets[0].Docker.SharedVolume)
	}
	if sets[0].Docker.Socket != DefaultDockerSocket {
		t.Errorf("sets[0].Docker.Socket = %q, want %s (inherited)", sets[0].Docker.Socket, DefaultDockerSocket)
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
	if sets[1].Docker.SharedVolume != "/data" {
		t.Errorf("sets[1].Docker.SharedVolume = %q, want /data (per-scaleset override)", sets[1].Docker.SharedVolume)
	}
	if sets[1].Docker.Socket != "/run/podman/podman.sock" {
		t.Errorf("sets[1].Docker.Socket = %q, want podman socket (per-scaleset override)", sets[1].Docker.Socket)
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
				c.Tart.Image = "macos-base:latest"
			},
		},
		{
			name: "tart missing image",
			modify: func(c *ScaleSetConfig) {
				c.Backend = "tart"
			},
			wantErr: "tart image is required",
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
		Defaults: ScaleSetConfig{
			RegistrationURL: "https://github.com/test-org",
			ScaleSetName:    "macos-runners",
			Token:           "ghp_test",
			MaxRunners:      2,
			Backend:         "tart",
			Tart: TartConfig{
				Image: "macos-base:latest",
			},
		},
	}

	sets := c.ResolveScaleSets()
	if len(sets) != 1 {
		t.Fatalf("expected 1 scale set, got %d", len(sets))
	}

	ss := sets[0]
	if ss.Tart.RunnerDir != DefaultTartRunnerDir {
		t.Errorf("Tart.RunnerDir = %q, want %q", ss.Tart.RunnerDir, DefaultTartRunnerDir)
	}
	if !ss.IsTart() {
		t.Error("IsTart() = false, want true")
	}
}

func TestResolveScaleSets_MixedBackends(t *testing.T) {
	c := Config{
		Defaults: ScaleSetConfig{
			MaxRunners: 5,
			Tart: TartConfig{
				Image: "global-macos:latest",
			},
		},
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
				Tart: TartConfig{
					Image: "custom-macos:latest",
				},
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
	if sets[0].Backend != DefaultBackend {
		t.Errorf("sets[0].Backend = %q, want %q", sets[0].Backend, DefaultBackend)
	}

	// Second: Tart with custom image
	if !sets[1].IsTart() {
		t.Error("sets[1] should be tart")
	}
	if sets[1].Tart.Image != "custom-macos:latest" {
		t.Errorf("sets[1].Tart.Image = %q, want custom", sets[1].Tart.Image)
	}
}

func TestNewSystemInfo(t *testing.T) {
	info := NewSystemInfo(42, "1.0.0")
	if info.ScaleSetID != 42 {
		t.Errorf("ScaleSetID = %d, want 42", info.ScaleSetID)
	}
	if info.System != DefaultSystemName {
		t.Errorf("System = %q, want %q", info.System, DefaultSystemName)
	}
	if info.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", info.Version, "1.0.0")
	}
}

func TestIsDinD_Default(t *testing.T) {
	ss := ScaleSetConfig{} // DinD is nil
	if !ss.IsDinD() {
		t.Error("IsDinD() with nil should return DefaultDinD (true)")
	}

	dindFalse := false
	ss.Docker.DinD = &dindFalse
	if ss.IsDinD() {
		t.Error("IsDinD() with explicit false should return false")
	}

	dindTrue := true
	ss.Docker.DinD = &dindTrue
	if !ss.IsDinD() {
		t.Error("IsDinD() with explicit true should return true")
	}
}

func TestApplyDefaults_BackendDefault(t *testing.T) {
	ss := ScaleSetConfig{} // Backend is ""
	ss.applyDefaults()
	if ss.Backend != DefaultBackend {
		t.Errorf("applyDefaults() Backend = %q, want %q", ss.Backend, DefaultBackend)
	}
}

func TestMergeDefaults(t *testing.T) {
	dindTrue := true
	defaults := ScaleSetConfig{
		RunnerImage: "default-image:latest",
		RunnerGroup: "default",
		MaxRunners:  10,
		Backend:     DefaultBackend,
		Docker: DockerConfig{
			Socket:       DefaultDockerSocket,
			DinD:         &dindTrue,
			SharedVolume: "/shared",
		},
		Tart: TartConfig{
			Image:     "global-macos:latest",
			RunnerDir: DefaultTartRunnerDir,
			CPU:       4,
			Memory:    8192,
			PoolSize:  2,
		},
	}

	// Scaleset with partial overrides
	dst := ScaleSetConfig{
		RegistrationURL: "https://github.com/org",
		ScaleSetName:    "test",
		Token:           "ghp_test",
		RunnerImage:     "custom-image:latest", // override
		// Everything else should be inherited
	}

	mergeDefaults(&dst, &defaults)

	if dst.RunnerImage != "custom-image:latest" {
		t.Errorf("RunnerImage should keep override, got %q", dst.RunnerImage)
	}
	if dst.RunnerGroup != "default" {
		t.Errorf("RunnerGroup should inherit, got %q", dst.RunnerGroup)
	}
	if dst.MaxRunners != 10 {
		t.Errorf("MaxRunners should inherit, got %d", dst.MaxRunners)
	}
	if dst.Docker.Socket != DefaultDockerSocket {
		t.Errorf("Docker.Socket should inherit, got %q", dst.Docker.Socket)
	}
	if !dst.IsDinD() {
		t.Error("IsDinD() should inherit true")
	}
	if dst.Docker.SharedVolume != "/shared" {
		t.Errorf("Docker.SharedVolume should inherit, got %q", dst.Docker.SharedVolume)
	}
	if dst.Tart.Image != "global-macos:latest" {
		t.Errorf("Tart.Image should inherit, got %q", dst.Tart.Image)
	}
	if dst.Tart.CPU != 4 {
		t.Errorf("Tart.CPU should inherit, got %d", dst.Tart.CPU)
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
