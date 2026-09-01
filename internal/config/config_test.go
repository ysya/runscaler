package config

import (
	"testing"
	"time"
)

func TestEffectiveDrainTimeout(t *testing.T) {
	zero := time.Duration(0)
	negative := -time.Second
	tenMin := 10 * time.Minute
	tests := []struct {
		name string
		set  *time.Duration
		want time.Duration
	}{
		{"unset inherits default", nil, DefaultDrainTimeout},
		{"explicit zero disables drain", &zero, 0},
		{"negative disables drain", &negative, negative},
		{"explicit value wins", &tenMin, tenMin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{DrainTimeout: tt.set}
			if got := c.EffectiveDrainTimeout(); got != tt.want {
				t.Errorf("EffectiveDrainTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func validScaleSetConfig() ScaleSetConfig {
	return ScaleSetConfig{
		RegistrationURL: "https://github.com/test-org",
		ScaleSetName:    "test-runners",
		Token:           "ghp_test",
		MaxRunners:      10,
		MinRunners:      0,
		Provider:        DefaultProvider,
		RunnerImage:     DefaultRunnerImage,
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

func TestScaleSetConfigValidateRejectsMalformedDockerPlatform(t *testing.T) {
	c := validScaleSetConfig()
	c.Docker.Platform = "linux"
	if err := c.Validate(); err == nil || !contains(err.Error(), "platform") {
		t.Fatalf("Validate() error = %v, want platform error", err)
	}
}

func TestValidateGlobal(t *testing.T) {
	valid := Config{LogLevel: "info", LogFormat: "text", HealthPort: 8080}
	if err := valid.ValidateGlobal(); err != nil {
		t.Fatalf("valid global config: %v", err)
	}
	for _, cfg := range []Config{
		{LogLevel: "verbose", LogFormat: "text", HealthPort: 8080},
		{LogLevel: "info", LogFormat: "yaml", HealthPort: 8080},
		{LogLevel: "info", LogFormat: "text", HealthPort: 70000},
	} {
		if err := cfg.ValidateGlobal(); err == nil {
			t.Fatalf("ValidateGlobal(%+v) succeeded, want error", cfg)
		}
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
	if sets[0].Provider != DefaultProvider {
		t.Errorf("Provider = %q, want %q", sets[0].Provider, DefaultProvider)
	}
}

// --- Tart provider validation tests ---

func TestScaleSetConfigValidate_TartProvider(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*ScaleSetConfig)
		wantErr string
	}{
		{
			name: "valid tart config",
			modify: func(c *ScaleSetConfig) {
				c.Provider = "tart"
				c.RunnerImage = "macos-base:latest"
				c.MaxRunners = 2
			},
		},
		{
			name: "tart exceeds host limit",
			modify: func(c *ScaleSetConfig) {
				c.Provider = "tart"
				c.RunnerImage = "macos-base:latest"
			},
			wantErr: "max-runners must be <= 2",
		},
		{
			name: "tart missing image",
			modify: func(c *ScaleSetConfig) {
				c.Provider = "tart"
				c.RunnerImage = ""
			},
			wantErr: "runner-image is required",
		},
		{
			name: "unsupported provider",
			modify: func(c *ScaleSetConfig) {
				c.Provider = "podman"
			},
			wantErr: "unsupported provider",
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
			Provider:        "tart",
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

	t.Run("long form entry with budget and on-exceed", func(t *testing.T) {
		dc := DockerConfig{Cache: []CacheVolumeSpec{
			{Name: "ccache", Path: "/home/runner/.ccache", Budget: "20GB", OnExceed: "wipe"},
		}}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() error: %v", err)
		}
		want := CacheVolumeMount{Volume: "ccache", Path: "/home/runner/.ccache", BudgetBytes: 20 * 1024 * 1024 * 1024, OnExceed: "wipe"}
		if len(mounts) != 1 || mounts[0] != want {
			t.Errorf("mounts = %+v, want [%+v]", mounts, want)
		}
	})

	t.Run("long form entry with no budget resolves to zero, same as the short form", func(t *testing.T) {
		dc := DockerConfig{Cache: []CacheVolumeSpec{{Name: "ccache", Path: "/home/runner/.ccache"}}}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() error: %v", err)
		}
		want := CacheVolumeMount{Volume: "ccache", Path: "/home/runner/.ccache"}
		if len(mounts) != 1 || mounts[0] != want {
			t.Errorf("mounts = %+v, want [%+v]", mounts, want)
		}
	})

	t.Run("same name in both forms: long form wins entirely, not merged field by field", func(t *testing.T) {
		dc := DockerConfig{
			CacheVolumes: []string{"ccache:/home/runner/.ccache-short"},
			Cache: []CacheVolumeSpec{
				{Name: "ccache", Path: "/home/runner/.ccache-long", Budget: "5GB", OnExceed: "warn"},
			},
		}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() error: %v", err)
		}
		want := CacheVolumeMount{Volume: "ccache", Path: "/home/runner/.ccache-long", BudgetBytes: 5 * 1024 * 1024 * 1024, OnExceed: "warn"}
		if len(mounts) != 1 || mounts[0] != want {
			t.Errorf("mounts = %+v, want [%+v] (the short-form entry must be fully discarded)", mounts, want)
		}
	})

	t.Run("short and long form entries with distinct names both survive, short form ordered first", func(t *testing.T) {
		dc := DockerConfig{
			CacheVolumes: []string{"gradle-cache:/home/runner/.gradle"},
			Cache:        []CacheVolumeSpec{{Name: "ccache", Path: "/home/runner/.ccache", Budget: "20GB"}},
		}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() error: %v", err)
		}
		if len(mounts) != 2 || mounts[0].Volume != "gradle-cache" || mounts[1].Volume != "ccache" {
			t.Errorf("mounts = %+v, want [gradle-cache, ccache] in that order", mounts)
		}
	})

	t.Run("on-exceed empty string is valid (behaves as warn downstream)", func(t *testing.T) {
		dc := DockerConfig{Cache: []CacheVolumeSpec{{Name: "x", Path: "/x", Budget: "1GB"}}}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() unexpected error: %v", err)
		}
		if len(mounts) != 1 || mounts[0].OnExceed != "" {
			t.Errorf("mounts = %+v, want OnExceed \"\" preserved as-is", mounts)
		}
	})

	t.Run("duplicate container path across the two forms", func(t *testing.T) {
		dc := DockerConfig{
			CacheVolumes: []string{"a:/shared-path"},
			Cache:        []CacheVolumeSpec{{Name: "b", Path: "/shared-path"}},
		}
		if _, err := dc.ParseCacheVolumes(); err == nil {
			t.Error("ParseCacheVolumes() expected a duplicate-path error")
		}
	})

	// Fix round 1: a rewrite regression let a repeated volume name within
	// one form silently keep only the last entry (cache-volumes =
	// ["cache:/a", "cache:/b"] used to yield two mounts, then started
	// yielding one) instead of erroring — and internal/provider/docker.go
	// mounts exactly what ParseCacheVolumes returns, so the dropped path
	// never reached the runner container with no warning anywhere. These
	// two pin that it is now a rejected config, not a silent drop, and the
	// third pins that the *intentional* same-name case (a long-form entry
	// overriding a short-form one) still isn't affected by that rejection.
	t.Run("duplicate volume name within the short form is rejected, not silently dropped", func(t *testing.T) {
		dc := DockerConfig{CacheVolumes: []string{"cache:/a", "cache:/b"}}
		mounts, err := dc.ParseCacheVolumes()
		if err == nil {
			t.Fatalf("ParseCacheVolumes() = %+v, want an error", mounts)
		}
		if !contains(err.Error(), "cache-volumes") || !contains(err.Error(), "duplicate") || !contains(err.Error(), "cache") {
			t.Errorf("error = %q, want it to name cache-volumes, duplicate, and the volume name %q", err.Error(), "cache")
		}
	})

	t.Run("duplicate volume name within the long form is rejected, not silently dropped", func(t *testing.T) {
		dc := DockerConfig{Cache: []CacheVolumeSpec{
			{Name: "ccache", Path: "/a"},
			{Name: "ccache", Path: "/b"},
		}}
		mounts, err := dc.ParseCacheVolumes()
		if err == nil {
			t.Fatalf("ParseCacheVolumes() = %+v, want an error", mounts)
		}
		if !contains(err.Error(), "[[docker.cache]]") || !contains(err.Error(), "duplicate") || !contains(err.Error(), "ccache") {
			t.Errorf("error = %q, want it to name [[docker.cache]], duplicate, and the volume name %q", err.Error(), "ccache")
		}
	})

	t.Run("cross-form same name is an override, not a rejected duplicate", func(t *testing.T) {
		dc := DockerConfig{
			CacheVolumes: []string{"ccache:/short-path"},
			Cache:        []CacheVolumeSpec{{Name: "ccache", Path: "/long-path", Budget: "1GB"}},
		}
		mounts, err := dc.ParseCacheVolumes()
		if err != nil {
			t.Fatalf("ParseCacheVolumes() unexpected error: %v", err)
		}
		want := CacheVolumeMount{Volume: "ccache", Path: "/long-path", BudgetBytes: 1024 * 1024 * 1024}
		if len(mounts) != 1 || mounts[0] != want {
			t.Errorf("mounts = %+v, want [%+v] (long form wins, cross-form repeat is not an error)", mounts, want)
		}
	})

	t.Run("long form validation errors", func(t *testing.T) {
		tests := []struct {
			name string
			spec CacheVolumeSpec
		}{
			{name: "missing name", spec: CacheVolumeSpec{Path: "/x"}},
			{name: "missing path", spec: CacheVolumeSpec{Name: "x"}},
			{name: "invalid volume name", spec: CacheVolumeSpec{Name: "bad/name", Path: "/x"}},
			{name: "relative path", spec: CacheVolumeSpec{Name: "x", Path: "relative"}},
			{name: "percentage budget rejected", spec: CacheVolumeSpec{Name: "x", Path: "/x", Budget: "10%"}},
			{name: "unparseable budget", spec: CacheVolumeSpec{Name: "x", Path: "/x", Budget: "lots"}},
			{name: "bad on-exceed", spec: CacheVolumeSpec{Name: "x", Path: "/x", OnExceed: "delete"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				dc := DockerConfig{Cache: []CacheVolumeSpec{tt.spec}}
				if mounts, err := dc.ParseCacheVolumes(); err == nil {
					t.Errorf("ParseCacheVolumes() expected error for %+v, got mounts %v", tt.spec, mounts)
				}
			})
		}
	})
}

func TestApplyDefaults_ProviderDefault(t *testing.T) {
	ss := ScaleSetConfig{} // Provider is ""
	ss.applyDefaults()
	if ss.Provider != DefaultProvider {
		t.Errorf("applyDefaults() Provider = %q, want %q", ss.Provider, DefaultProvider)
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

func TestDaemonWideDockerCleanupDefaultsOff(t *testing.T) {
	var ss ScaleSetConfig
	if ss.IsBuildxCleanupEnabled() {
		t.Error("buildx cleanup must be opt-in")
	}
	if ss.IsDockerPruneEnabled() {
		t.Error("Docker prune must be opt-in")
	}

	on := true
	ss.Docker.BuildxCleanup = &on
	ss.Docker.Prune = &on
	if !ss.IsBuildxCleanupEnabled() || !ss.IsDockerPruneEnabled() {
		t.Error("explicit cleanup opt-in was ignored")
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

func TestIsUpdateDisabled(t *testing.T) {
	tr, fa := true, false
	tests := []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset inherits default", nil, DefaultDisableUpdate},
		{"explicit true", &tr, true},
		{"explicit false allows self-update", &fa, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ss := ScaleSetConfig{DisableUpdate: tt.set}
			if got := ss.IsUpdateDisabled(); got != tt.want {
				t.Errorf("IsUpdateDisabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDiskConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		disk    DiskConfig
		wantErr string
	}{
		{name: "defaults are valid", disk: DiskConfig{}},
		{name: "min >= target rejected",
			disk:    DiskConfig{MinFree: "30%", TargetFree: "20%"},
			wantErr: "min-free must be less than target-free"},
		{name: "equal rejected",
			disk:    DiskConfig{MinFree: "20%", TargetFree: "20%"},
			wantErr: "min-free must be less than target-free"},
		{name: "bad threshold rejected",
			disk:    DiskConfig{MinFree: "lots"},
			wantErr: "invalid threshold"},
		{name: "bad target threshold rejected",
			disk:    DiskConfig{TargetFree: "lots"},
			wantErr: "invalid threshold"},
		{name: "max-tier out of range",
			disk:    DiskConfig{MaxTier: 9},
			wantErr: "max-tier must be between 1 and 4"},
		{name: "max-tier negative rejected",
			disk:    DiskConfig{MaxTier: -1},
			wantErr: "max-tier must be between 1 and 4"},
		{name: "max-tier 1 through 4 all valid",
			disk: DiskConfig{MaxTier: 1}},
		{name: "mixed units rejected",
			disk:    DiskConfig{MinFree: "10%", TargetFree: "20GB"},
			wantErr: "must both be percentages or both be sizes"},
		{name: "byte thresholds compared correctly",
			disk: DiskConfig{MinFree: "10GB", TargetFree: "20GB"}},
		{name: "byte thresholds min >= target rejected",
			disk:    DiskConfig{MinFree: "2048MB", TargetFree: "1GB"}, // 2048MB == 2GB > 1GB
			wantErr: "min-free must be less than target-free"},
		{name: "only min-free overridden still compared against default target",
			disk:    DiskConfig{MinFree: "25%"}, // default target-free is 20%
			wantErr: "min-free must be less than target-free"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.disk.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestIsDiskGuardEnabled(t *testing.T) {
	tr, fa := true, false
	tests := []struct {
		name string
		set  *bool
		want bool
	}{
		{"unset inherits default", nil, DefaultDiskGuard},
		{"explicit true", &tr, true},
		{"explicit false disables the guard", &fa, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Disk: DiskConfig{Guard: tt.set}}
			if got := c.IsDiskGuardEnabled(); got != tt.want {
				t.Errorf("IsDiskGuardEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestValidateGlobal_RejectsInvalidDisk pins down that ValidateGlobal — the
// single call site used by both `runner validate` and `runner run` — fails
// on a bad [disk] section rather than merely warning (see DiskConfig's
// Validate doc comment for why an inverted MinFree/TargetFree pair is more
// than cosmetic: it can underflow the guard's shortfall calculation).
func TestValidateGlobal_RejectsInvalidDisk(t *testing.T) {
	cfg := Config{LogLevel: "info", LogFormat: "text", HealthPort: 8080,
		Disk: DiskConfig{MinFree: "20%", TargetFree: "10%"}}
	err := cfg.ValidateGlobal()
	if err == nil || !contains(err.Error(), "min-free must be less than target-free") {
		t.Errorf("ValidateGlobal() = %v, want min-free/target-free error", err)
	}
}
