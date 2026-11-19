package config

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func decodeRegistryAuth(t *testing.T, auth string) map[string]string {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(auth)
	if err != nil {
		t.Fatalf("RegistryAuth() is not base64url: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("RegistryAuth() is not JSON: %v", err)
	}
	return decoded
}

func TestDockerConfig_RegistryAuth_AnonymousWithoutCredentials(t *testing.T) {
	auth, err := DockerConfig{}.RegistryAuth()
	if err != nil || auth != "" {
		t.Fatalf("RegistryAuth() = %q, %v; want empty and no error", auth, err)
	}
}

func TestDockerConfig_RegistryAuth_EncodesLiteralCredentials(t *testing.T) {
	auth, err := DockerConfig{RegistryUsername: "user", RegistryPassword: "secret", RegistryServer: "registry.example.com"}.RegistryAuth()
	if err != nil {
		t.Fatalf("RegistryAuth() error: %v", err)
	}
	got := decodeRegistryAuth(t, auth)
	if got["username"] != "user" || got["password"] != "secret" || got["serveraddress"] != "registry.example.com" {
		t.Errorf("decoded auth = %v", got)
	}
}

func TestDockerConfig_RegistryAuth_ReadsEnvReferences(t *testing.T) {
	t.Setenv("TEST_REGISTRY_USER", "env-user")
	t.Setenv("TEST_REGISTRY_TOKEN", "env-token")
	auth, err := DockerConfig{RegistryUsername: "env:TEST_REGISTRY_USER", RegistryPassword: "env:TEST_REGISTRY_TOKEN"}.RegistryAuth()
	if err != nil {
		t.Fatalf("RegistryAuth() error: %v", err)
	}
	got := decodeRegistryAuth(t, auth)
	if got["username"] != "env-user" || got["password"] != "env-token" {
		t.Errorf("decoded auth = %v", got)
	}
}

func TestDockerConfig_RegistryAuth_RejectsIncompleteOrUnsetCredentials(t *testing.T) {
	t.Setenv("TEST_REGISTRY_EMPTY", "")
	tests := []struct {
		name    string
		docker  DockerConfig
		wantErr string
	}{
		{"password missing", DockerConfig{RegistryUsername: "user"}, "docker.registry-password"},
		{"username missing", DockerConfig{RegistryPassword: "secret"}, "docker.registry-username"},
		{"unset variable", DockerConfig{RegistryUsername: "user", RegistryPassword: "env:TEST_REGISTRY_NOT_SET"}, "TEST_REGISTRY_NOT_SET"},
		{"empty variable", DockerConfig{RegistryUsername: "user", RegistryPassword: "env:TEST_REGISTRY_EMPTY"}, "TEST_REGISTRY_EMPTY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth, err := tt.docker.RegistryAuth()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RegistryAuth() = %q, %v; want an error mentioning %q", auth, err, tt.wantErr)
			}
			if auth != "" {
				t.Errorf("RegistryAuth() returned credentials alongside an error: %q", auth)
			}
		})
	}
}

func TestScaleSetConfig_Validate_ReportsUnusableRegistryCredentials(t *testing.T) {
	ss := validScaleSetConfig()
	ss.Docker.RegistryUsername = "user"
	err := ss.Validate()
	if err == nil || !strings.Contains(err.Error(), "docker.registry-password") {
		t.Fatalf("Validate() = %v; want an error about docker.registry-password", err)
	}
	ss.Docker.RegistryPassword = "secret"
	if err := ss.Validate(); err != nil {
		t.Fatalf("Validate() with both credentials = %v", err)
	}
}
