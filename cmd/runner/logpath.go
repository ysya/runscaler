package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/layout"
)

// logWriter keeps a nil *LogFileWriter from becoming a non-nil io.Writer,
// which made every log line panic when file logging was off.
func logWriter(f *config.LogFileWriter) io.Writer {
	if f == nil {
		return nil
	}
	return f
}

// logFileDecision is where runner run writes its own log.
type logFileDecision struct {
	Path     string // file to open
	Fallback string // opened when Path cannot be; "" when none
	Enabled  bool
	Warning  string // why a configured path was overridden or logging is off
}

// resolveLogFile decides the log file without touching the filesystem. Root
// accepts an explicit path only under the same /var/log rule the system unit
// uses, and keeps the default as a fallback for when opening it fails.
func resolveLogFile(cfg config.Config, id layout.Identity) logFileDecision {
	if cfg.LogFile != nil && *cfg.LogFile == "" {
		return logFileDecision{}
	}
	lay, layoutErr := layout.For(id)
	if cfg.LogFile == nil {
		if layoutErr != nil {
			return logFileDecision{Warning: fmt.Sprintf("file logging disabled: no default log location: %v", layoutErr)}
		}
		return logFileDecision{Path: lay.LogFile, Enabled: true}
	}
	path := *cfg.LogFile
	if !id.Root {
		return logFileDecision{Path: path, Enabled: true}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if _, ok := layout.SystemLogsDirectory(path); !ok {
		return logFileDecision{
			Path:    lay.LogFile,
			Enabled: true,
			Warning: fmt.Sprintf("log-file %s ignored: root logs must be in a directory under %s named with letters, digits, '.', '_' or '-'; writing to %s", path, layout.SystemLogRoot, lay.LogFile),
		}
	}
	return logFileDecision{Path: path, Fallback: lay.LogFile, Enabled: true}
}

// openLogFileFor opens a log for id: root walks from / without following
// symlinks and vets every directory below /var/log.
var openLogFileFor = func(id layout.Identity, path string) (*config.LogFileWriter, error) {
	if id.Root {
		return config.OpenRootLogFile(path, layout.SystemLogRoot, id.DirPerm())
	}
	return config.OpenLogFile(path, id.DirPerm())
}

// openRunLog opens the decided log, trying the fallback once, and returns the
// path it opened with the warnings to print before the logger exists.
func openRunLog(id layout.Identity, d logFileDecision) (*config.LogFileWriter, string, []string) {
	var warnings []string
	if d.Warning != "" {
		warnings = append(warnings, d.Warning)
	}
	if !d.Enabled {
		return nil, "", warnings
	}
	w, err := openLogFileFor(id, d.Path)
	if err == nil {
		return w, d.Path, warnings
	}
	warnings = append(warnings, fmt.Sprintf("cannot open log file %s: %v", d.Path, err))
	if d.Fallback == "" || d.Fallback == d.Path {
		return nil, "", warnings
	}
	if w, err = openLogFileFor(id, d.Fallback); err != nil {
		warnings = append(warnings, fmt.Sprintf("cannot open log file %s either: %v; continuing without a log file", d.Fallback, err))
		return nil, "", warnings
	}
	warnings = append(warnings, "writing to "+d.Fallback+" instead")
	return w, d.Fallback, warnings
}

// oldDefaultLogFile returns the log older releases wrote beside the config
// when it still exists and is not where runner writes now.
func oldDefaultLogFile(cfg config.Config, usedConfig, current string) string {
	if cfg.LogFile != nil || usedConfig == "" || current == "" {
		return ""
	}
	old := filepath.Join(filepath.Dir(usedConfig), config.DefaultLogFileName)
	if sameFilePath(old, current) {
		return ""
	}
	if info, err := os.Stat(old); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return old
}
