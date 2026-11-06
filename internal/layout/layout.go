// Package layout decides where runner keeps its files: FHS locations for
// root and the XDG base directories for everyone else, with macOS logs under
// ~/Library/Logs where Console.app and launchd expect them.
package layout

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	SystemConfigFile = "/etc/runner/config.toml"
	SystemLogRoot    = "/var/log"
	SystemLogFile    = "/var/log/runner/runner.log"
	SystemBackupDir  = "/var/lib/runner/backups"
)

var errNoHome = errors.New("$HOME is not set")

// Identity is everything that decides where runner keeps its files.
type Identity struct {
	GOOS          string // "linux" or "darwin"
	Root          bool   // effective UID is 0
	Home          string // $HOME as seen by this process; "" when unset
	XDGConfigHome string // honored only when absolute
	XDGStateHome  string // honored only when absolute
}

// Layout holds the default locations for one identity.
type Layout struct {
	ConfigFile string
	LogFile    string
	BackupDir  string
}

// CurrentIdentity describes the running process. It never fails: root needs
// no home directory, and users need one only where XDG does not apply.
func CurrentIdentity() Identity {
	home, _ := os.UserHomeDir()
	return Identity{
		GOOS:          runtime.GOOS,
		Root:          os.Geteuid() == 0,
		Home:          home,
		XDGConfigHome: os.Getenv("XDG_CONFIG_HOME"),
		XDGStateHome:  os.Getenv("XDG_STATE_HOME"),
	}
}

// ConfigHome is $XDG_CONFIG_HOME when absolute, otherwise ~/.config.
func (id Identity) ConfigHome() (string, error) {
	if filepath.IsAbs(id.XDGConfigHome) {
		return id.XDGConfigHome, nil
	}
	return id.underHome(".config")
}

// StateHome is $XDG_STATE_HOME when absolute, otherwise ~/.local/state.
func (id Identity) StateHome() (string, error) {
	if filepath.IsAbs(id.XDGStateHome) {
		return id.XDGStateHome, nil
	}
	return id.underHome(".local", "state")
}

func (id Identity) underHome(elem ...string) (string, error) {
	if !filepath.IsAbs(id.Home) {
		return "", errNoHome
	}
	return filepath.Join(append([]string{id.Home}, elem...)...), nil
}

// DirPerm is the mode for directories runner creates: world-readable system
// directories, private per-user ones as the XDG specification requires.
func (id Identity) DirPerm() os.FileMode {
	if id.Root {
		return 0o755
	}
	return 0o700
}

// For returns the default locations for id. Root never fails; a user fails
// only when a needed base directory has neither $HOME nor an absolute XDG
// variable.
func For(id Identity) (Layout, error) {
	if id.Root {
		return Layout{ConfigFile: SystemConfigFile, LogFile: SystemLogFile, BackupDir: SystemBackupDir}, nil
	}
	configHome, err := id.ConfigHome()
	if err != nil {
		return Layout{}, err
	}
	stateHome, err := id.StateHome()
	if err != nil {
		return Layout{}, err
	}
	logFile := filepath.Join(stateHome, "runner", "runner.log")
	if id.GOOS == "darwin" {
		if logFile, err = id.underHome("Library", "Logs", "runner", "runner.log"); err != nil {
			return Layout{}, err
		}
	}
	return Layout{
		ConfigFile: filepath.Join(configHome, "runner", "config.toml"),
		LogFile:    logFile,
		BackupDir:  filepath.Join(stateHome, "runner", "backups"),
	}, nil
}

var logsDirectoryName = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// SystemLogsDirectory reports logFile's directory relative to SystemLogRoot
// when it is strictly below it and every component uses only characters
// systemd's LogsDirectory= accepts unescaped. The system unit and root's
// runtime log decision share this rule so they always agree.
func SystemLogsDirectory(logFile string) (string, bool) {
	if !filepath.IsAbs(logFile) {
		return "", false
	}
	rel, err := filepath.Rel(SystemLogRoot, filepath.Dir(filepath.Clean(logFile)))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || !logsDirectoryName.MatchString(rel) {
		return "", false
	}
	return rel, true
}
