package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

func TestRetiredConfigKeysFailWithoutUsingTheirValues(t *testing.T) {
	for _, tc := range []struct{ name, body, key, replacement string }{
		{"flat provider", "backend='private-retired-value'\n", "backend", "provider"},
		{"flat docker", "[docker]\nshared-volume-ttl='24h'\n", "shared-volume-ttl", "shared-volume-max-age"},
		{"flat tart", "[tart]\ncache-space-budget=123\n", "cache-space-budget", "cache-budget"},
		{"multi provider", "[[scaleset]]\nbackend='private-retired-value'\n", "backend", "provider"},
		{"multi docker", "[[scaleset]]\n[scaleset.docker]\nshared-volume-ttl='24h'\n", "shared-volume-ttl", "shared-volume-max-age"},
		{"multi tart", "[[scaleset]]\n[scaleset.tart]\ncache-space-budget=123\n", "cache-space-budget", "cache-budget"},
		{"both spellings", "provider='docker'\nbackend='private-retired-value'\n", "backend", "provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := viper.New()
			v.SetConfigType("toml")
			if err := v.ReadConfig(strings.NewReader(tc.body)); err != nil {
				t.Fatal(err)
			}
			_, err := Load(v)
			if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.replacement) || strings.Contains(err.Error(), "private-retired-value") {
				t.Fatalf("retired key error=%v", err)
			}
		})
	}
}

func TestRetiredTokenEnvironmentIsNotAnImplicitFallback(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "")
	t.Setenv("RUNSCALER_TOKEN", "synthetic-retired-token")
	ss := ScaleSetConfig{}
	ss.resolveEnvToken()
	if ss.Token != "" {
		t.Fatal("retired environment variable still selects authentication")
	}
}

func TestRetiredStructAliasesAreRemoved(t *testing.T) {
	if _, ok := reflect.TypeFor[DockerConfig]().FieldByName("SharedVolumeTTL"); ok {
		t.Error("retired SharedVolumeTTL field remains")
	}
	if _, ok := reflect.TypeFor[TartConfig]().FieldByName("CacheSpaceBudgetGB"); ok {
		t.Error("retired CacheSpaceBudgetGB field remains")
	}
}
