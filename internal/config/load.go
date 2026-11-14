package config

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

// globalOnlyKeys are top-level keys that configure the process rather than a
// scale set; they are never inherited by [[scaleset]] entries.
var globalOnlyKeys = []string{"log-level", "log-format", "log-file", "health-port", "health-address", "dry-run", "drain-timeout", "concurrent", "diagnostics", "actions-cache"}

// identityKeys identify a single scale set (or only make sense per scale
// set, like min-runners where 0 is a valid explicit value); they are never
// inherited from the top level.
var identityKeys = []string{"url", "name", "token", "github-app", "labels", "min-runners"}

// Retired spellings are errors rather than ignored keys: silently dropping a
// provider or retention setting could run jobs with different behavior.
func rejectRetiredKeys(settings map[string]any) error {
	for _, key := range []struct{ table, old, current string }{
		{"", "backend", "provider"},
		{"docker", "shared-volume-ttl", "shared-volume-max-age"},
		{"tart", "cache-space-budget", "cache-budget"},
	} {
		table := settings
		prefix := ""
		if key.table != "" {
			var ok bool
			table, ok = settings[key.table].(map[string]any)
			if !ok {
				continue
			}
			prefix = key.table + "."
		}
		if _, exists := table[key.old]; exists {
			return fmt.Errorf("config key %q has been removed; use %q", prefix+key.old, prefix+key.current)
		}
	}
	return nil
}

// Load builds a Config from the given viper instance. Inheritance for
// [[scaleset]] entries happens here at the map level: a key present in a
// scaleset entry always wins, even when set to a zero value (e.g.
// `memory = 0` or `dind = false` override a non-zero default).
//
// Unknown keys and mixed single/multi-mode keys go into Config.Warnings;
// validation treats them as errors while older deployments can still start.
// Sunset notices go into Config.Deprecations so they never block validation.
func Load(v *viper.Viper) (Config, error) {
	settings := v.AllSettings()
	// AllSettings drops empty tables while flattening configuration. Preserve
	// explicit App selection so an empty App table fails validation instead
	// of silently falling back to an ambient personal access token.
	if _, present := settings["github-app"]; !present && v.IsSet("github-app") {
		settings["github-app"] = v.Get("github-app")
	}

	rawScaleSets, err := popScaleSets(settings)
	if err != nil {
		return Config{}, err
	}

	if err := rejectRetiredKeys(settings); err != nil {
		return Config{}, err
	}
	settings = deepMerge(map[string]any{"retry": defaultRetrySettings()}, settings)
	var warnings []string

	var cfg Config
	topUnused, err := decodeStrict(settings, &cfg)
	if err != nil {
		return Config{}, err
	}
	deprecations := cacheVolumeSunsetWarnings(cfg.Defaults.Docker, settings, "")

	var scaleSetWarnings []string

	if len(rawScaleSets) > 0 {
		// Defaults every [[scaleset]] entry inherits: compiled-in defaults
		// overlaid with the top-level settings (minus global-only and
		// identity keys). Each entry is then overlaid on top of that base.
		base := deepMerge(builtinDefaults(), scalesetDefaults(settings))

		cfg.ScaleSets = make([]ScaleSetConfig, 0, len(rawScaleSets))
		for i, entry := range rawScaleSets {
			if err := rejectRetiredKeys(entry); err != nil {
				return Config{}, fmt.Errorf("scaleset[%d]: %w", i, err)
			}

			var ss ScaleSetConfig
			unused, err := decodeStrict(deepMerge(base, entry), &ss)
			if err != nil {
				return Config{}, fmt.Errorf("scaleset[%d]: %w", i, err)
			}
			deprecations = append(deprecations,
				cacheVolumeSunsetWarnings(ss.Docker, entry, fmt.Sprintf("scaleset[%d]: ", i))...)
			for _, key := range unused {
				// Unknown keys inherited from the top level are already
				// reported by the top-level decode; only report keys this
				// entry sets itself.
				if hasKeyPath(entry, key) {
					scaleSetWarnings = append(scaleSetWarnings, fmt.Sprintf(
						"scaleset[%d]: unknown config key %q — ignored (check for typos; run 'runner validate')", i, key))
				}
			}
			cfg.ScaleSets = append(cfg.ScaleSets, ss)
		}

		// Single-mode keys are silently ignored in multi mode — warn so a
		// mixed config does not quietly drop a scale set.
		if keys := singleModeKeysSet(settings); len(keys) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"top-level %s are ignored when [[scaleset]] entries exist — move them into a [[scaleset]]",
				strings.Join(keys, "/")))
		}
	}

	for _, key := range topUnused {
		warnings = append(warnings, fmt.Sprintf(
			"unknown config key %q — ignored (check for typos; run 'runner validate')", key))
	}
	warnings = append(warnings, scaleSetWarnings...)

	cfg.Warnings = warnings
	cfg.Deprecations = append(deprecations, nativeActionsCacheURLSunsetWarnings(cfg)...)
	if cfg.HealthAddress == "" {
		cfg.HealthAddress = DefaultHealthAddress
	}
	return cfg, nil
}

func cacheVolumeSunsetWarnings(docker DockerConfig, source map[string]any, prefix string) []string {
	var warnings []string
	for _, field := range []struct {
		key  string
		used bool
	}{
		{"cache-volumes", len(docker.CacheVolumes) > 0},
		{"cache", len(docker.Cache) > 0},
	} {
		if field.used && hasKeyPath(source, "docker."+field.key) {
			warnings = append(warnings, prefix+"docker."+field.key+
				" is deprecated; migrate tool caches to workflow Actions cache before removing this volume mount")
		}
	}
	return warnings
}

func nativeActionsCacheURLSunsetWarnings(cfg Config) []string {
	if !cfg.ActionsCache.Enabled || cfg.ActionsCache.AdvertiseURL == "" {
		return nil
	}
	warning := "docker.actions-cache-url is deprecated when it points to [actions-cache].advertise-url; use docker.native-actions-cache = true and remove the duplicate URL before a future breaking release"
	if len(cfg.ScaleSets) == 0 {
		if (cfg.Defaults.Provider == "" || cfg.Defaults.Provider == DefaultProvider) && cfg.Defaults.Docker.ActionsCacheURL == cfg.ActionsCache.AdvertiseURL {
			return []string{warning}
		}
		return nil
	}
	var warnings []string
	for i, ss := range cfg.ScaleSets {
		if ss.Provider == DefaultProvider && ss.Docker.ActionsCacheURL == cfg.ActionsCache.AdvertiseURL {
			warnings = append(warnings, fmt.Sprintf("scaleset[%d]: %s", i, warning))
		}
	}
	return warnings
}

// popScaleSets removes the "scaleset" key from settings and returns its
// entries. Viper hands back []any (config files) or []map[string]any
// (programmatic Set), so both are accepted.
func popScaleSets(settings map[string]any) ([]map[string]any, error) {
	raw, ok := settings["scaleset"]
	if !ok {
		return nil, nil
	}
	delete(settings, "scaleset")

	switch arr := raw.(type) {
	case []map[string]any:
		return arr, nil
	case []any:
		entries := make([]map[string]any, 0, len(arr))
		for i, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("scaleset[%d]: expected a table, got %T", i, e)
			}
			entries = append(entries, m)
		}
		return entries, nil
	default:
		return nil, fmt.Errorf("scaleset: expected an array of tables, got %T", raw)
	}
}

// builtinDefaults returns the compiled-in scale set defaults as a map, so
// Load produces sane scale sets even when no CLI flag bindings seeded the
// viper instance (tests, library use). Retry defaults are seeded once into
// the top-level settings (see Load), which every [[scaleset]] inherits.
// Retention defaults are applied by the accessors in retention.go and are
// deliberately not seeded here, so an unset value stays visibly unset.
func builtinDefaults() map[string]any {
	return map[string]any{
		"runner-image": DefaultRunnerImage,
		"runner-group": DefaultRunnerGroup,
		"max-runners":  DefaultMaxRunners,
		"provider":     DefaultProvider,
		"docker": map[string]any{
			"socket": DefaultDockerSocket,
			"dind":   DefaultDinD,
		},
	}
}

// scalesetDefaults returns a copy of the top-level settings with the keys
// that must never be inherited by [[scaleset]] entries removed.
func scalesetDefaults(settings map[string]any) map[string]any {
	out := maps.Clone(settings)
	for _, key := range globalOnlyKeys {
		delete(out, key)
	}
	for _, key := range identityKeys {
		delete(out, key)
	}
	return out
}

// deepMerge returns a new map with override overlaid on base: when both
// sides hold a map for the same key they merge recursively, otherwise the
// override value wins. Neither input is mutated.
func deepMerge(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	maps.Copy(out, base)
	for k, v := range override {
		if baseMap, ok := out[k].(map[string]any); ok {
			if overrideMap, ok := v.(map[string]any); ok {
				out[k] = deepMerge(baseMap, overrideMap)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// decodeStrict decodes src into dst mirroring viper v1.21's default decoder
// config (weakly typed input, duration strings, comma-separated strings to
// slices) and returns the sorted input keys that matched no struct field.
// Nested keys come back dotted (e.g. "docker.foo").
func decodeStrict[T any](src map[string]any, dst *T) ([]string, error) {
	var md mapstructure.Metadata
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Metadata:         &md,
		Result:           dst,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			stringToWeakSliceHookFunc(","),
		),
	})
	if err != nil {
		return nil, err
	}
	if err := dec.Decode(src); err != nil {
		return nil, err
	}
	return leafUnusedKeys(md.Unused), nil
}

// stringToWeakSliceHookFunc mirrors viper's private hook of the same name:
// like mapstructure.StringToSliceHookFunc but without requiring the target
// element type to be string, so decoding matches viper.Unmarshal exactly.
func stringToWeakSliceHookFunc(sep string) mapstructure.DecodeHookFunc {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String || t.Kind() != reflect.Slice {
			return data, nil
		}
		raw := data.(string)
		if raw == "" {
			return []string{}, nil
		}
		return strings.Split(raw, sep), nil
	}
}

// leafUnusedKeys sorts and deduplicates Metadata.Unused, dropping any key
// that is a parent of another unused entry. mapstructure reports an unknown
// nested table as a single parent key without descending into it (pinned by
// TestLoad_UnknownKeys), so the parent filter is purely defensive.
func leafUnusedKeys(unused []string) []string {
	if len(unused) == 0 {
		return nil
	}
	keys := slices.Clone(unused)
	slices.Sort(keys)
	keys = slices.Compact(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		parent := false
		for _, other := range keys {
			if strings.HasPrefix(other, key+".") {
				parent = true
				break
			}
		}
		if !parent {
			out = append(out, key)
		}
	}
	return out
}

// hasKeyPath reports whether the dotted key path exists in m.
func hasKeyPath(m map[string]any, dotted string) bool {
	cur := any(m)
	for _, part := range strings.Split(dotted, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = mm[part]; !ok {
			return false
		}
	}
	return true
}

// singleModeKeysSet returns which single-mode identity keys carry a non-zero
// value in the top-level settings. Flag bindings inject zero defaults
// (url = "", min-runners = 0, ...) into AllSettings even when nothing was
// set, so key presence alone is not meaningful — only non-zero values are.
func singleModeKeysSet(settings map[string]any) []string {
	var keys []string
	for _, key := range identityKeys {
		if isExplicitlySet(settings[key]) {
			keys = append(keys, key)
		}
	}
	return keys
}

// isExplicitlySet reports whether v holds a non-zero value.
func isExplicitlySet(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case int:
		return x != 0
	case int32:
		return x != 0
	case int64:
		return x != 0
	case uint:
		return x != 0
	case uint32:
		return x != 0
	case uint64:
		return x != 0
	case float32:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	default:
		// Unexpected non-nil types (e.g. a table where a scalar belongs)
		// still count as explicitly set.
		return true
	}
}
