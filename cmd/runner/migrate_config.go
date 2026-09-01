package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2/unstable"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/config"
)

const backupManifestSchema = 1

var (
	migrationNow = time.Now
	filenameSafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
)

type configMigrationPaths struct {
	Source       string
	Target       string
	BackupDir    string
	Mode         string
	DirPerm      os.FileMode
	RemoveSource bool
}

type configMigrationResult struct {
	Found            bool
	Source           string
	Target           string
	BackupPath       string
	ManifestPath     string
	TargetBackupPath string
	Changes          []string
	BackupCreated    bool
	TargetChanged    bool
	TargetCreated    bool
	RemoveSource     bool
}

type configBackupManifest struct {
	SchemaVersion   int      `json:"schema_version"`
	CreatedAt       string   `json:"created_at"`
	Mode            string   `json:"mode"`
	RunnerVersion   string   `json:"runner_version"`
	RunnerCommit    string   `json:"runner_commit,omitempty"`
	RunnerBuildDate string   `json:"runner_build_date,omitempty"`
	OriginalPath    string   `json:"original_path"`
	MigrationTarget string   `json:"migration_target"`
	BackupPath      string   `json:"backup_path"`
	ConfigSHA256    string   `json:"config_sha256"`
	Changes         []string `json:"changes,omitempty"`
	RestoreCommand  string   `json:"restore_command"`
}

type keyOccurrence struct {
	scope     int
	path      []string
	keyOffset int
	keyLength int
	newKey    string
}

type byteReplacement struct {
	offset int
	length int
	value  []byte
}

// canonicalizeDeprecatedConfig rewrites only deprecated TOML key tokens. The
// parser supplies exact byte ranges, so comments, ordering, whitespace, quoted
// values, and secrets remain byte-for-byte unchanged.
func canonicalizeDeprecatedConfig(data []byte) ([]byte, []string, error) {
	parser := unstable.Parser{}
	parser.Reset(data)
	var (
		currentTable []string
		scaleSetID   int
		currentScope int
		occurrences  []keyOccurrence
		seen         = make(map[string]struct{})
	)

	for parser.NextExpression() {
		expr := parser.Expression()
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			currentTable = nodeKey(expr)
			if expr.Kind == unstable.ArrayTable && slicesEqual(currentTable, []string{"scaleset"}) {
				scaleSetID++
				currentScope = scaleSetID
			} else if len(currentTable) == 0 || currentTable[0] != "scaleset" {
				currentScope = 0
			}
		case unstable.KeyValue:
			keyNodes, keyParts := nodeKeyParts(expr)
			path := append(append([]string(nil), currentTable...), keyParts...)
			seen[scopedKey(currentScope, path)] = struct{}{}
			alias, ok := deprecatedAliasForPath(path)
			if !ok {
				continue
			}
			last := keyNodes[len(keyNodes)-1]
			occurrences = append(occurrences, keyOccurrence{
				scope:     currentScope,
				path:      path,
				keyOffset: int(last.Raw.Offset),
				keyLength: int(last.Raw.Length),
				newKey:    alias.NewKey,
			})
		}
	}
	if err := parser.Error(); err != nil {
		return nil, nil, fmt.Errorf("parse TOML for migration: %w", err)
	}

	replacements := make([]byteReplacement, 0, len(occurrences))
	changes := make([]string, 0, len(occurrences))
	for _, occurrence := range occurrences {
		canonicalPath := append([]string(nil), occurrence.path...)
		canonicalPath[len(canonicalPath)-1] = occurrence.newKey
		if _, conflict := seen[scopedKey(occurrence.scope, canonicalPath)]; conflict {
			return nil, nil, fmt.Errorf("deprecated key %s and canonical key %s are both set; remove one before migrating",
				strings.Join(occurrence.path, "."), strings.Join(canonicalPath, "."))
		}
		raw := data[occurrence.keyOffset : occurrence.keyOffset+occurrence.keyLength]
		replacements = append(replacements, byteReplacement{
			offset: occurrence.keyOffset,
			length: occurrence.keyLength,
			value:  renderMigratedKey(raw, occurrence.newKey),
		})
		changes = append(changes, fmt.Sprintf("%s -> %s",
			strings.Join(occurrence.path, "."), strings.Join(canonicalPath, ".")))
	}

	if len(replacements) == 0 {
		return bytes.Clone(data), nil, nil
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].offset < replacements[j].offset })
	var out bytes.Buffer
	last := 0
	for _, replacement := range replacements {
		out.Write(data[last:replacement.offset])
		out.Write(replacement.value)
		last = replacement.offset + replacement.length
	}
	out.Write(data[last:])
	return out.Bytes(), changes, nil
}

func nodeKey(node *unstable.Node) []string {
	_, parts := nodeKeyParts(node)
	return parts
}

func nodeKeyParts(node *unstable.Node) ([]*unstable.Node, []string) {
	var nodes []*unstable.Node
	var parts []string
	iterator := node.Key()
	for iterator.Next() {
		key := iterator.Node()
		nodes = append(nodes, key)
		parts = append(parts, string(key.Data))
	}
	return nodes, parts
}

func deprecatedAliasForPath(path []string) (config.DeprecatedKeyAlias, bool) {
	for _, alias := range config.DeprecatedKeyAliases() {
		if len(path) == 0 || path[len(path)-1] != alias.OldKey {
			continue
		}
		prefix := path[:len(path)-1]
		if alias.Table == "" {
			if len(prefix) == 0 || slicesEqual(prefix, []string{"scaleset"}) {
				return alias, true
			}
			continue
		}
		if slicesEqual(prefix, []string{alias.Table}) || slicesEqual(prefix, []string{"scaleset", alias.Table}) {
			return alias, true
		}
	}
	return config.DeprecatedKeyAlias{}, false
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func scopedKey(scope int, path []string) string {
	return fmt.Sprintf("%d:%s", scope, strings.Join(path, "\x00"))
}

func renderMigratedKey(raw []byte, key string) []byte {
	if len(raw) >= 2 && (raw[0] == '\'' || raw[0] == '"') && raw[len(raw)-1] == raw[0] {
		return []byte(string(raw[0]) + key + string(raw[0]))
	}
	return []byte(key)
}

// validateMigratedConfig performs the complete offline validation used before
// any service or config mutation. Connectivity checks intentionally remain in
// `runner validate`; migration must be usable during an outage.
func validateMigratedConfig(data []byte) error {
	v := viper.New()
	v.SetConfigType("toml")
	v.SetDefault("log-level", config.DefaultLogLevel)
	v.SetDefault("log-format", config.DefaultLogFormat)
	v.SetDefault("runner-image", config.DefaultRunnerImage)
	v.SetDefault("runner-group", config.DefaultRunnerGroup)
	v.SetDefault("max-runners", config.DefaultMaxRunners)
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		return fmt.Errorf("parse migrated config: %w", err)
	}
	cfg, err := config.Load(v)
	if err != nil {
		return fmt.Errorf("decode migrated config: %w", err)
	}
	if len(cfg.Warnings) > 0 {
		return fmt.Errorf("migrated config is not strict: %s", strings.Join(cfg.Warnings, "; "))
	}
	if err := cfg.ValidateGlobal(); err != nil {
		return fmt.Errorf("validate global config: %w", err)
	}
	sets := cfg.ResolveScaleSets()
	if len(sets) == 0 {
		return errors.New("migrated config has no scale sets")
	}
	for i := range sets {
		if err := sets[i].Validate(); err != nil {
			return fmt.Errorf("validate scaleset[%d] %q: %w", i, sets[i].ScaleSetName, err)
		}
	}
	if err := cfg.ValidateConcurrent(sets); err != nil {
		return fmt.Errorf("validate global capacity: %w", err)
	}
	if err := validateScaleSetCollection(sets); err != nil {
		return fmt.Errorf("validate scale set collection: %w", err)
	}
	return nil
}

func migrateConfigFile(paths configMigrationPaths, dryRun bool) (configMigrationResult, error) {
	result := configMigrationResult{
		Source:       paths.Source,
		Target:       paths.Target,
		RemoveSource: paths.RemoveSource,
	}
	sourceData, err := os.ReadFile(paths.Source)
	if err != nil {
		if !os.IsNotExist(err) {
			return result, fmt.Errorf("read source config: %w", err)
		}
		// A completed/partial prior migration may have already removed the old
		// source. Canonicalize and validate the target in place on re-runs.
		sourceData, err = os.ReadFile(paths.Target)
		if err != nil {
			if os.IsNotExist(err) {
				return result, nil
			}
			return result, fmt.Errorf("read target config: %w", err)
		}
		result.Source = paths.Target
		result.RemoveSource = false
	}

	migratedData, changes, err := canonicalizeDeprecatedConfig(sourceData)
	if err != nil {
		return result, err
	}
	if err := validateMigratedConfig(migratedData); err != nil {
		return result, err
	}
	result.Changes = changes
	result.Found = true

	targetData, targetErr := os.ReadFile(paths.Target)
	targetExists := targetErr == nil
	if targetErr != nil && !os.IsNotExist(targetErr) {
		return result, fmt.Errorf("read existing target config: %w", targetErr)
	}
	samePath := sameFilePath(result.Source, paths.Target)
	if targetExists && !samePath && !bytes.Equal(targetData, migratedData) {
		if dryRun {
			return result, fmt.Errorf("target config %s already exists with different content; dry-run will not overwrite it", paths.Target)
		}
		if err := ensureDir(filepath.Dir(paths.Target), paths.DirPerm); err != nil {
			return result, err
		}
		result.BackupPath, result.ManifestPath, result.BackupCreated, err = createConfigBackup(
			result.Source, paths.Target, paths.BackupDir, paths.Mode, sourceData, changes)
		if err != nil {
			return result, err
		}
		result.TargetBackupPath, _, _, err = createConfigBackup(
			paths.Target, paths.Target, paths.BackupDir, paths.Mode, targetData, nil)
		if err != nil {
			return result, err
		}
		return result, fmt.Errorf("target config %s differs from migrated source; neither was overwritten (source backup: %s, target backup: %s)",
			paths.Target, result.BackupPath, result.TargetBackupPath)
	}

	result.TargetChanged = !targetExists || !bytes.Equal(targetData, migratedData)
	result.TargetCreated = !targetExists
	if dryRun {
		return result, nil
	}
	if err := ensureDir(filepath.Dir(paths.Target), paths.DirPerm); err != nil {
		return result, err
	}
	result.BackupPath, result.ManifestPath, result.BackupCreated, err = createConfigBackup(
		result.Source, paths.Target, paths.BackupDir, paths.Mode, sourceData, changes)
	if err != nil {
		return result, err
	}
	if samePath && result.TargetChanged {
		result.TargetBackupPath = result.BackupPath
	}
	if result.TargetChanged {
		if err := writeFileAtomic(paths.Target, migratedData, 0o600); err != nil {
			return result, fmt.Errorf("write migrated config: %w", err)
		}
	}
	return result, nil
}

func createConfigBackup(originalPath, migrationTarget, backupDir, mode string, data []byte, changes []string) (backupPath, manifestPath string, created bool, err error) {
	if err := ensureDir(backupDir, 0o700); err != nil {
		return "", "", false, fmt.Errorf("create backup directory: %w", err)
	}
	contentHash := sha256Hex(data)
	pathHash := sha256Hex([]byte(filepath.Clean(originalPath)))[:8]
	pattern := filepath.Join(backupDir, fmt.Sprintf("config.toml.pre-migrate-*-%s-%s.bak", contentHash[:12], pathHash))
	if matches, globErr := filepath.Glob(pattern); globErr == nil {
		for _, candidate := range matches {
			candidateData, readErr := os.ReadFile(candidate)
			candidateManifest := candidate + ".manifest.json"
			if readErr == nil && sha256Hex(candidateData) == contentHash {
				if _, statErr := os.Stat(candidateManifest); statErr == nil {
					return candidate, candidateManifest, false, nil
				}
			}
		}
	}

	createdAt := migrationNow().UTC()
	backupName := fmt.Sprintf("config.toml.pre-migrate-%s-%s-%s-%s.bak",
		migrationVersionLabel(), createdAt.Format("20060102T150405Z"), contentHash[:12], pathHash)
	backupPath = filepath.Join(backupDir, backupName)
	manifestPath = backupPath + ".manifest.json"
	if err := writeFileAtomic(backupPath, data, 0o600); err != nil {
		return "", "", false, fmt.Errorf("write config backup: %w", err)
	}
	verified, err := os.ReadFile(backupPath)
	if err != nil || sha256Hex(verified) != contentHash {
		return backupPath, "", true, fmt.Errorf("verify config backup %s: checksum mismatch", backupPath)
	}
	manifest := configBackupManifest{
		SchemaVersion:   backupManifestSchema,
		CreatedAt:       createdAt.Format(time.RFC3339),
		Mode:            mode,
		RunnerVersion:   nonEmpty(version, "dev"),
		RunnerCommit:    commit,
		RunnerBuildDate: date,
		OriginalPath:    originalPath,
		MigrationTarget: migrationTarget,
		BackupPath:      backupPath,
		ConfigSHA256:    contentHash,
		Changes:         append([]string(nil), changes...),
		RestoreCommand:  fmt.Sprintf("install -m 600 %s %s", shellQuotePath(backupPath), shellQuotePath(migrationTarget)),
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return backupPath, "", true, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestData = append(manifestData, '\n')
	if err := writeFileAtomic(manifestPath, manifestData, 0o600); err != nil {
		return backupPath, manifestPath, true, fmt.Errorf("write backup manifest: %w", err)
	}
	return backupPath, manifestPath, true, nil
}

func rollbackConfigMigration(result configMigrationResult) error {
	if !result.TargetChanged {
		return nil
	}
	if result.TargetCreated {
		if err := os.Remove(result.Target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove newly created config %s: %w", result.Target, err)
		}
		return nil
	}
	if result.TargetBackupPath == "" {
		return fmt.Errorf("cannot restore %s: target backup is missing", result.Target)
	}
	data, err := os.ReadFile(result.TargetBackupPath)
	if err != nil {
		return fmt.Errorf("read target backup: %w", err)
	}
	if err := writeFileAtomic(result.Target, data, 0o600); err != nil {
		return fmt.Errorf("restore target config: %w", err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runner-migrate-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func ensureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("set directory permissions on %s: %w", path, err)
	}
	return nil
}

func migrationVersionLabel() string {
	label := strings.TrimSpace(version)
	if label == "" || label == "dev" {
		label = "dev"
		if commit != "" {
			label += "-" + shortCommit(commit)
		}
	} else if label[0] >= '0' && label[0] <= '9' {
		label = "v" + label
	}
	label = strings.Trim(filenameSafe.ReplaceAllString(label, "-"), "-._")
	if label == "" {
		return "dev"
	}
	if len(label) > 64 {
		return label[:64]
	}
	return label
}

func shortCommit(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shellQuotePath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

func sameFilePath(a, b string) bool {
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr == nil && bErr == nil {
		return filepath.Clean(aAbs) == filepath.Clean(bAbs)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
