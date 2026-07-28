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
		{
			name: "valid cache volumes",
			modify: func(c *ScaleSetConfig) {
				c.Docker.CacheVolumes = []string{"gradle-cache:/home/runner/.gradle"}
			},
		},
		{
			name: "bad cache volumes",
			modify: func(c *ScaleSetConfig) {
				c.Docker.CacheVolumes = []string{"gradle-cache"}
			},
			wantErr: "cache-volumes entry",
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
				c.RunnerImage = "macos-base:latest"
			},
		},
		{
			name: "tart missing image",
			modify: func(c *ScaleSetConfig) {
				c.Backend = "tart"
				c.RunnerImage = ""
			},
			wantErr: "runner-image is required",
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
			RunnerImage:     "macos-base:latest",
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

func TestSharedVolumeName_Default(t *testing.T) {
	ss := ScaleSetConfig{} // SharedVolumeName is ""
	if got := ss.SharedVolumeName(); got != DefaultSharedVolumeName {
		t.Errorf("SharedVolumeName() = %q, want default %q", got, DefaultSharedVolumeName)
	}

	ss.Docker.SharedVolumeName = "team-a-shared"
	if got := ss.SharedVolumeName(); got != "team-a-shared" {
		t.Errorf("SharedVolumeName() = %q, want explicit override", got)
	}
}

func TestParseCacheVolumes(t *testing.T) {
	t.Run("empty list parses to nil", func(t *testing.T) {
		dc := DockerConfig{}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() error: %v", err)
		}
		if mounts != nil {
			t.Errorf("mounts = %v, want nil", mounts)
		}
	})

	t.Run("happy path splits on the first colon", func(t *testing.T) {
		dc := DockerConfig{CacheVolumes: []string{
			"gradle-cache:/home/runner/.gradle",
			"pnpm-store:/home/runner/.local/share/pnpm/store",
			"go-build-cache:/home/runner/.cache/go-build",
		}}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() error: %v", err)
		}
		want := []CacheVolumeMount{
			{Volume: "gradle-cache", Path: "/home/runner/.gradle"},
			{Volume: "pnpm-store", Path: "/home/runner/.local/share/pnpm/store"},
			{Volume: "go-build-cache", Path: "/home/runner/.cache/go-build"},
		}
		if len(mounts) != len(want) {
			t.Fatalf("mounts = %v, want %v", mounts, want)
		}
		for i := range want {
			if mounts[i] != want[i] {
				t.Errorf("mounts[%d] = %+v, want %+v", i, mounts[i], want[i])
			}
		}
	})

	t.Run("error cases", func(t *testing.T) {
		tests := []struct {
			name  string
			entry string
		}{
			{name: "no colon", entry: "gradle-cache"},
			{name: "empty volume name", entry: ":/home/runner/.gradle"},
			{name: "empty path", entry: "gradle-cache:"},
			{name: "relative path", entry: "gradle-cache:home/runner/.gradle"},
			{name: "invalid volume name character", entry: "gradle/cache:/home/runner/.gradle"},
			{name: "volume name starts with separator", entry: "-gradle:/home/runner/.gradle"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				dc := DockerConfig{CacheVolumes: []string{tt.entry}}
				mounts, err := dc.ParseCacheVolumes()
				if err == nil {
					t.Fatalf("ParseCacheVolumes(%q) expected error, got %v", tt.entry, mounts)
				}
				if !contains(err.Error(), tt.entry) {
					t.Errorf("error %q should reference the bad entry %q", err.Error(), tt.entry)
				}
				if mounts != nil {
					t.Errorf("mounts = %v, want nil on error", mounts)
				}
			})
		}
	})
}

func TestApplyDefaults_BackendDefault(t *testing.T) {
	ss := ScaleSetConfig{} // Backend is ""
	ss.applyDefaults()
	if ss.Backend != DefaultBackend {
		t.Errorf("applyDefaults() Backend = %q, want %q", ss.Backend, DefaultBackend)
	}
}

func TestIsTartCacheCleanupEnabled(t *testing.T) {
	tr, fa := true, false

	// nil → default true (safety net is on unless explicitly disabled).
	var ss ScaleSetConfig
	if !ss.IsTartCacheCleanupEnabled() {
		t.Errorf("nil CacheCleanup should default to enabled")
	}
	ss.Tart.CacheCleanup = &tr
	if !ss.IsTartCacheCleanupEnabled() {
		t.Errorf("explicit true should be enabled")
	}
	ss.Tart.CacheCleanup = &fa
	if ss.IsTartCacheCleanupEnabled() {
		t.Errorf("explicit false should be disabled")
	}
}

func TestResolveEnvTokenPrefersRunnerToken(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "new-token")
	t.Setenv("RUNSCALER_TOKEN", "legacy-token")
	ss := &ScaleSetConfig{}
	ss.resolveEnvToken()
	if ss.Token != "new-token" {
		t.Errorf("Token = %q, want %q (RUNNER_TOKEN must win)", ss.Token, "new-token")
	}
}

func TestResolveEnvTokenFallsBackToLegacy(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "") // unset/empty → must fall back
	t.Setenv("RUNSCALER_TOKEN", "legacy-token")
	ss := &ScaleSetConfig{}
	ss.resolveEnvToken()
	if ss.Token != "legacy-token" {
		t.Errorf("Token = %q, want %q (legacy RUNSCALER_TOKEN fallback)", ss.Token, "legacy-token")
	}
}

func TestResolveEnvTokenExplicitEnvRef(t *testing.T) {
	t.Setenv("MY_CUSTOM_TOKEN", "custom")
	ss := &ScaleSetConfig{Token: "env:MY_CUSTOM_TOKEN"}
	ss.resolveEnvToken()
	if ss.Token != "custom" {
		t.Errorf("Token = %q, want %q (env: ref)", ss.Token, "custom")
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
