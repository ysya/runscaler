package main

import (
	"testing"

	"github.com/actions/scaleset"
)

func validConfig() Config {
	return Config{
		RegistrationURL: "https://github.com/test-org",
		ScaleSetName:    "test-runners",
		Token:           "ghp_test",
		MaxRunners:      10,
		MinRunners:      0,
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr string
	}{
		{
			name:   "valid config",
			modify: func(c *Config) {},
		},
		{
			name:    "missing url",
			modify:  func(c *Config) { c.RegistrationURL = "" },
			wantErr: "registration URL",
		},
		{
			name:    "invalid url",
			modify:  func(c *Config) { c.RegistrationURL = "not-a-url" },
			wantErr: "invalid registration URL",
		},
		{
			name:    "missing name",
			modify:  func(c *Config) { c.ScaleSetName = "" },
			wantErr: "scale set name",
		},
		{
			name:    "missing token",
			modify:  func(c *Config) { c.Token = "" },
			wantErr: "token",
		},
		{
			name:    "negative min runners",
			modify:  func(c *Config) { c.MinRunners = -1 },
			wantErr: "min-runners must be >= 0",
		},
		{
			name:    "zero max runners",
			modify:  func(c *Config) { c.MaxRunners = 0 },
			wantErr: "max-runners must be >= 1",
		},
		{
			name: "min exceeds max",
			modify: func(c *Config) {
				c.MinRunners = 5
				c.MaxRunners = 3
			},
			wantErr: "min-runners (5) must be <= max-runners (3)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
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
		c := Config{
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
		c := Config{
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

func TestSystemInfo(t *testing.T) {
	info := systemInfo(42)
	if info.ScaleSetID != 42 {
		t.Errorf("ScaleSetID = %d, want 42", info.ScaleSetID)
	}
	if info.System != "dockerscaleset" {
		t.Errorf("System = %q, want %q", info.System, "dockerscaleset")
	}
	if info == (scaleset.SystemInfo{}) {
		t.Error("systemInfo() returned zero value")
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1048576, "1.0 MiB"},
		{1073741824, "1.0 GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := formatBytes(tt.input)
			if got != tt.want {
				t.Errorf("formatBytes(%d) = %q, want %q", tt.input, got, tt.want)
			}
		})
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
