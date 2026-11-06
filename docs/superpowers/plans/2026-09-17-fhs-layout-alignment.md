# 目錄配置對齊業界慣例（FHS / XDG / Apple）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 讓 runner 的 config、log、備份、binary 依執行身分放在平台慣例的位置，並修正讓 service 起不來、會 panic 或可被提權的相關問題。

**Architecture:** 新增純函式套件 `internal/layout` 作為預設路徑與 root log 目錄規則的單一來源，`internal/pathtrust` 作為「只有 root 能改」的單一判斷。log 檔改為相對於已開啟的目錄 fd 寫入與輪替；root 從 `/` 逐層以 `O_NOFOLLOW` 開啟目錄。service 範本改為 systemd `LogsDirectory=` 沙箱與 launchd 的 stderr-only 記錄，`service install` 與 `migrate` 共用同一套安裝準備、驗證與產生流程。

**Tech Stack:** Go 1.26；`golang.org/x/sys/unix`；`spf13/cobra`、`spf13/viper`；`charm.land/log/v2`；systemd、launchd。

**Spec:** `docs/superpowers/specs/2026-09-16-fhs-layout-alignment-design.md`

## Global Constraints

- 預設路徑（spec A）：root → config `/etc/runner/config.toml`、log `/var/log/runner/runner.log`、備份 `/var/lib/runner/backups`；Linux 使用者 → `$XDG_CONFIG_HOME|~/.config/runner/config.toml`、`$XDG_STATE_HOME|~/.local/state/runner/runner.log`、`$XDG_STATE_HOME|~/.local/state/runner/backups`；macOS 使用者 → config 與備份同 Linux 使用者，log `~/Library/Logs/runner/runner.log`
- XDG 變數只接受絕對路徑；`XDG_CONFIG_HOME` 為絕對路徑時解析使用者 config 目錄不得要求 `$HOME`
- 身分以 effective UID 判斷；lock 路徑維持 `/tmp/runner.lock`
- 建立目錄權限：使用者層級 0700、root 0755；log 檔 0640；config 檔 0600
- root log 只能位於 `layout.SystemLogsDirectory` 接受的 `/var/log` 子目錄（元件只含 `[A-Za-z0-9._-]`）
- macOS 上 effective UID 0：拒絕 `runner run`、`service install|start|restart`、`migrate`（任何 scope）；允許 `service uninstall|stop|status|logs`
- service 範本環境變數：`RUNNER_SERVICE_VERSION=2`、`RUNNER_SERVICE_STOP_TIMEOUT=<drain-timeout + 60 秒，單位秒>`
- systemd `ExecStart` 參數跳脫 `\`、`"`、`%`→`%%`、`$`→`$$`；`Environment=` 值跳脫 `\`、`"`、`%`；含控制字元一律拒絕
- launchd label：目前 `io.github.ysya.runner`、舊版 `com.runscaler.agent`
- 所有安裝 service 的入口必須在呼叫 service manager 前完成 config 載入、binary 驗證與定義產生
- 不自動搬移任何既有檔案
- 程式碼註解一律英文；conventional commits，不加 attribution
- 所有工作在分支 `feat/fhs-layout-alignment` 進行
- 每個 task 結束前必須全綠：`go build ./... && go test ./... -count=1 && go vet ./... && gofmt -l .`（需無輸出）`&& golangci-lint run ./...`（0 issues）
- 若執行環境禁止前景 `sleep`，含等待的驗證指令改以背景執行並等待完成通知

---

## 檔案結構

| 檔案 | 職責 |
|---|---|
| `internal/layout/layout.go` | 新增。`Identity`、`Layout`、`For`、`CurrentIdentity`、`SystemLogsDirectory` |
| `internal/pathtrust/pathtrust.go` | 新增。`Problem(uid uint32, mode fs.FileMode) string` |
| `cmd/runner/trusted_path.go` | 新增。路徑版檢查：`fileStat`、`statFunc`、`lstatFile`、`checkRootOnly`、`checkRootOnlyChain` |
| `internal/config/logfile.go` | 目錄 fd 的 `LogFileWriter`、`OpenLogFile`、`OpenRootLogFile` |
| `internal/lock/lock.go` | 跨身分開檔流程 |
| `cmd/runner/logpath.go` | 新增。`logWriter`、`resolveLogFile`、`openRunLog`、`oldDefaultLogFile`、`stdoutIsDevNull`、`logConsole` |
| `cmd/runner/service_env.go` | 新增。範本環境變數常數、過舊 service 提醒、重裝指令 |
| `cmd/runner/platform_guard.go` | 新增。`refuseDarwinRoot`、`pathExists` |
| `cmd/runner/cmd_service.go` | 範本與跳脫、`readServiceConfig`、`buildInstallOpts`、`validateServiceBinary`、`installService`、`--force`、uninstall 舊版、logs／status |
| `cmd/runner/cmd_migrate.go` | `splitSystemdCommand`、共用安裝流程、備份預設、macOS 拒絕 |
| `cmd/runner/cmd_init.go`、`cmd_logs.go`、`cmd_update.go`、`config_paths.go`、`loadconfig.go`、`legacy.go`、`main.go`、`tool_path.go` | 改用上述單元 |
| `install.sh`、`README.md`、`config.example.toml` | 安裝目錄與文件 |

---

### Task 0: 建立分支並提交既有修正

**Files:**
- 既有未提交：`cmd/runner/tool_path.go`、`cmd/runner/tool_path_test.go`、`cmd/runner/cmd_service.go`、`cmd/runner/cmd_service_test.go`、`cmd/runner/main.go`、`cmd/runner/main_test.go`、`README.md`
- 新增：`docs/superpowers/specs/2026-09-16-fhs-layout-alignment-design.md`、`docs/superpowers/plans/2026-09-17-fhs-layout-alignment.md`

**Interfaces:**
- Consumes: 無
- Produces: 分支 `feat/fhs-layout-alignment`；既有的 `ensureHomebrewPath()`、`appendMissingDirs(path string, dirs []string) string`、`explainLockError(err error) error`

- [ ] **Step 1: 確認工作樹只有預期的變更**

Run: `git status --short`
Expected: 只列出上方 Files 的檔案

- [ ] **Step 2: 確認全綠**

Run: `go build ./... && go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./...`
Expected: 測試全部 PASS，`gofmt -l` 無輸出，lint 0 issues

- [ ] **Step 3: 建立分支**

Run: `git switch -c feat/fhs-layout-alignment`

- [ ] **Step 4: 提交 spec 與計畫**

```bash
git add docs/superpowers/specs/2026-09-16-fhs-layout-alignment-design.md docs/superpowers/plans/2026-09-17-fhs-layout-alignment.md
git commit -m "docs(spec): design and plan for FHS/XDG layout alignment"
```

- [ ] **Step 5: 提交既有修正**

```bash
git add cmd/runner/tool_path.go cmd/runner/tool_path_test.go cmd/runner/cmd_service.go cmd/runner/cmd_service_test.go cmd/runner/main.go cmd/runner/main_test.go README.md
git commit -m "fix(service): unblock the systemd lock and find Homebrew tart under launchd"
```

---

### Task 1: `internal/layout` 預設路徑與 root log 目錄規則

**Files:**
- Create: `internal/layout/layout.go`
- Create: `internal/layout/layout_test.go`
- Modify: `cmd/runner/config_paths.go`（`userConfigPath`）
- Modify: `cmd/runner/cmd_service.go:25`（`defaultConfigPath`）

**Interfaces:**
- Consumes: 無
- Produces:
  - `const layout.SystemConfigFile = "/etc/runner/config.toml"`、`layout.SystemLogRoot = "/var/log"`、`layout.SystemLogFile = "/var/log/runner/runner.log"`、`layout.SystemBackupDir = "/var/lib/runner/backups"`
  - `type layout.Identity struct { GOOS string; Root bool; Home, XDGConfigHome, XDGStateHome string }`
  - `func layout.CurrentIdentity() layout.Identity`
  - `func (id layout.Identity) ConfigHome() (string, error)`、`StateHome() (string, error)`、`DirPerm() os.FileMode`
  - `type layout.Layout struct { ConfigFile, LogFile, BackupDir string }`
  - `func layout.For(id layout.Identity) (layout.Layout, error)`
  - `func layout.SystemLogsDirectory(logFile string) (string, bool)`

- [ ] **Step 1: 寫失敗測試**

`internal/layout/layout_test.go`：

```go
package layout

import (
	"os"
	"testing"
)

func TestFor(t *testing.T) {
	tests := []struct {
		name    string
		id      Identity
		want    Layout
		wantErr bool
	}{
		{
			name: "linux root needs no home",
			id:   Identity{GOOS: "linux", Root: true, XDGStateHome: "/xdg/state"},
			want: Layout{ConfigFile: "/etc/runner/config.toml", LogFile: "/var/log/runner/runner.log", BackupDir: "/var/lib/runner/backups"},
		},
		{
			name: "linux user defaults",
			id:   Identity{GOOS: "linux", Home: "/home/ada"},
			want: Layout{ConfigFile: "/home/ada/.config/runner/config.toml", LogFile: "/home/ada/.local/state/runner/runner.log", BackupDir: "/home/ada/.local/state/runner/backups"},
		},
		{
			name: "linux user absolute XDG",
			id:   Identity{GOOS: "linux", Home: "/home/ada", XDGConfigHome: "/xdg/config", XDGStateHome: "/xdg/state"},
			want: Layout{ConfigFile: "/xdg/config/runner/config.toml", LogFile: "/xdg/state/runner/runner.log", BackupDir: "/xdg/state/runner/backups"},
		},
		{
			name: "linux user relative XDG is ignored",
			id:   Identity{GOOS: "linux", Home: "/home/ada", XDGConfigHome: "rel/config", XDGStateHome: "rel/state"},
			want: Layout{ConfigFile: "/home/ada/.config/runner/config.toml", LogFile: "/home/ada/.local/state/runner/runner.log", BackupDir: "/home/ada/.local/state/runner/backups"},
		},
		{
			name: "linux user without home but with absolute XDG",
			id:   Identity{GOOS: "linux", XDGConfigHome: "/xdg/config", XDGStateHome: "/xdg/state"},
			want: Layout{ConfigFile: "/xdg/config/runner/config.toml", LogFile: "/xdg/state/runner/runner.log", BackupDir: "/xdg/state/runner/backups"},
		},
		{
			name:    "linux user without home or XDG",
			id:      Identity{GOOS: "linux"},
			wantErr: true,
		},
		{
			name: "darwin user logs under Library",
			id:   Identity{GOOS: "darwin", Home: "/Users/ada", XDGStateHome: "/xdg/state"},
			want: Layout{ConfigFile: "/Users/ada/.config/runner/config.toml", LogFile: "/Users/ada/Library/Logs/runner/runner.log", BackupDir: "/xdg/state/runner/backups"},
		},
		{
			name:    "darwin user without home cannot place logs",
			id:      Identity{GOOS: "darwin", XDGConfigHome: "/xdg/config", XDGStateHome: "/xdg/state"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := For(tt.id)
			if (err != nil) != tt.wantErr {
				t.Fatalf("For() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("For() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConfigHomeNeedsNoHomeWithAbsoluteXDG(t *testing.T) {
	if got, err := (Identity{XDGConfigHome: "/xdg/config"}).ConfigHome(); err != nil || got != "/xdg/config" {
		t.Errorf("ConfigHome() = %q, %v", got, err)
	}
	if _, err := (Identity{XDGConfigHome: "relative"}).ConfigHome(); err == nil {
		t.Error("ConfigHome() without home or absolute XDG succeeded")
	}
}

func TestSystemLogsDirectory(t *testing.T) {
	tests := []struct {
		logFile string
		want    string
		ok      bool
	}{
		{logFile: "/var/log/runner/runner.log", want: "runner", ok: true},
		{logFile: "/var/log/ci/runner/runner.log", want: "ci/runner", ok: true},
		{logFile: "/var/log/runner.log"},
		{logFile: "/var/logs/runner/runner.log"},
		{logFile: "/home/ada/runner/runner.log"},
		{logFile: "runner/runner.log"},
		{logFile: "/var/log/has space/runner.log"},
		{logFile: "/var/log/100%/runner.log"},
		{logFile: "/var/log/runner/../../etc/runner.log"},
	}
	for _, tt := range tests {
		got, ok := SystemLogsDirectory(tt.logFile)
		if got != tt.want || ok != tt.ok {
			t.Errorf("SystemLogsDirectory(%q) = %q, %v; want %q, %v", tt.logFile, got, ok, tt.want, tt.ok)
		}
	}
}

func TestDirPerm(t *testing.T) {
	if got := (Identity{Root: true}).DirPerm(); got != os.FileMode(0o755) {
		t.Errorf("root DirPerm = %o, want 755", got)
	}
	if got := (Identity{}).DirPerm(); got != os.FileMode(0o700) {
		t.Errorf("user DirPerm = %o, want 700", got)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./internal/layout -count=1`
Expected: FAIL，`undefined: Identity`（套件尚未存在的編譯錯誤）

- [ ] **Step 3: 實作**

`internal/layout/layout.go`：

```go
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
```

- [ ] **Step 4: 確認測試通過**

Run: `go test ./internal/layout -count=1 -v`
Expected: 全部 PASS

- [ ] **Step 5: `userConfigPath` 與 `defaultConfigPath` 改用 layout**

`cmd/runner/config_paths.go` 的 `userConfigPath` 換成：

```go
// Use the same CLI convention on Linux and macOS. Relative XDG paths are
// invalid under the XDG specification and must not depend on the working dir.
func userConfigPath(app string) (string, error) {
	base, err := layout.CurrentIdentity().ConfigHome()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, app, "config.toml"), nil
}
```

import 為 `"fmt"`、`"path/filepath"`、`"runtime"`、`"github.com/ysya/runscaler/internal/layout"`（移除 `"os"`）。

`cmd/runner/cmd_service.go` 常數區塊改為 `defaultConfigPath = layout.SystemConfigFile`，並 import `"github.com/ysya/runscaler/internal/layout"`。

- [ ] **Step 6: 確認既有測試仍通過**

Run: `go test ./cmd/runner -count=1 -run 'TestUserConfigOperations|TestMigrationDiscoversXDGUserConfig|TestServiceConfigPrecedence'`
Expected: PASS

- [ ] **Step 7: 全綠檢查並提交**

```bash
git add internal/layout cmd/runner/config_paths.go cmd/runner/cmd_service.go
git commit -m "feat(layout): centralize default config, log, and backup paths"
```

---

### Task 2: `internal/pathtrust` 與路徑版 root 專屬檢查

**Files:**
- Create: `internal/pathtrust/pathtrust.go`
- Create: `internal/pathtrust/pathtrust_test.go`
- Create: `cmd/runner/trusted_path.go`
- Create: `cmd/runner/trusted_path_test.go`

**Interfaces:**
- Consumes: 無
- Produces:
  - `func pathtrust.Problem(uid uint32, mode fs.FileMode) string`
  - `type fileStat struct { UID uint32; Mode fs.FileMode }`、`type statFunc func(path string) (fileStat, error)`
  - `func lstatFile(path string) (fileStat, error)`
  - `type untrustedPathError struct { Path, Reason string }`
  - `func checkRootOnly(path string, stat statFunc) error`、`func checkRootOnlyChain(path string, stat statFunc) error`
  - 測試 helper（後續 task 的測試會用）：`func fakeStat(entries map[string]fileStat) statFunc`、`func rootDir(perm fs.FileMode) fileStat`

- [ ] **Step 1: 寫失敗測試**

`internal/pathtrust/pathtrust_test.go`：

```go
package pathtrust

import (
	"io/fs"
	"testing"
)

func TestProblem(t *testing.T) {
	tests := []struct {
		name string
		uid  uint32
		mode fs.FileMode
		want string
	}{
		{name: "root-only file", uid: 0, mode: 0o755},
		{name: "root-only directory", uid: 0, mode: fs.ModeDir | 0o755},
		{name: "user owned", uid: 1000, mode: 0o755, want: "is owned by uid 1000, not root"},
		{name: "group writable", uid: 0, mode: 0o775, want: "is writable by its group"},
		{name: "others writable", uid: 0, mode: 0o757, want: "is writable by others"},
		{name: "symlink", uid: 0, mode: fs.ModeSymlink | 0o777, want: "is a symlink"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Problem(tt.uid, tt.mode); got != tt.want {
				t.Errorf("Problem() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

`cmd/runner/trusted_path_test.go`：

```go
package main

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeStat(entries map[string]fileStat) statFunc {
	return func(path string) (fileStat, error) {
		st, ok := entries[path]
		if !ok {
			return fileStat{}, &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrNotExist}
		}
		return st, nil
	}
}

func rootDir(perm fs.FileMode) fileStat {
	return fileStat{UID: 0, Mode: fs.ModeDir | perm}
}

func TestCheckRootOnlyChain(t *testing.T) {
	safe := map[string]fileStat{
		"/":                     rootDir(0o755),
		"/usr":                  rootDir(0o755),
		"/usr/local":            rootDir(0o755),
		"/usr/local/bin":        rootDir(0o755),
		"/usr/local/bin/runner": {UID: 0, Mode: 0o755},
	}
	with := func(path string, st fileStat) map[string]fileStat {
		entries := maps.Clone(safe)
		entries[path] = st
		return entries
	}
	tests := []struct {
		name       string
		entries    map[string]fileStat
		wantPath   string
		wantReason string
	}{
		{name: "root-owned chain", entries: safe},
		{name: "binary owned by a user", entries: with("/usr/local/bin/runner", fileStat{UID: 1000, Mode: 0o755}), wantPath: "/usr/local/bin/runner", wantReason: "owned by uid 1000"},
		{name: "group-writable binary", entries: with("/usr/local/bin/runner", fileStat{UID: 0, Mode: 0o775}), wantPath: "/usr/local/bin/runner", wantReason: "writable by its group"},
		{name: "others-writable ancestor", entries: with("/usr/local", rootDir(0o757)), wantPath: "/usr/local", wantReason: "writable by others"},
		{name: "user-owned ancestor", entries: with("/usr/local/bin", fileStat{UID: 1000, Mode: fs.ModeDir | 0o755}), wantPath: "/usr/local/bin", wantReason: "owned by uid 1000"},
		{name: "symlinked component", entries: with("/usr/local/bin/runner", fileStat{UID: 0, Mode: fs.ModeSymlink | 0o777}), wantPath: "/usr/local/bin/runner", wantReason: "is a symlink"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkRootOnlyChain("/usr/local/bin/runner", fakeStat(tt.entries))
			if tt.wantPath == "" {
				if err != nil {
					t.Fatalf("checkRootOnlyChain() = %v, want nil", err)
				}
				return
			}
			var untrusted *untrustedPathError
			if !errors.As(err, &untrusted) || untrusted.Path != tt.wantPath || !strings.Contains(untrusted.Reason, tt.wantReason) {
				t.Fatalf("checkRootOnlyChain() = %v, want %s %s", err, tt.wantPath, tt.wantReason)
			}
		})
	}
}

func TestCheckRootOnlyChainPropagatesStatErrors(t *testing.T) {
	err := checkRootOnlyChain("/usr/local/bin/runner", fakeStat(map[string]fileStat{}))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("checkRootOnlyChain() = %v, want fs.ErrNotExist", err)
	}
}

func TestCheckRootOnlyChainRejectsRelativePaths(t *testing.T) {
	if err := checkRootOnlyChain("bin/runner", fakeStat(nil)); err == nil {
		t.Fatal("relative path passed")
	}
}

func TestCheckRootOnlyChainRealFiles(t *testing.T) {
	// Any file others can write fails, whoever owns it and wherever it lives.
	writable := filepath.Join(t.TempDir(), "runner")
	if err := os.WriteFile(writable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := checkRootOnlyChain(writable, lstatFile); err == nil {
		t.Fatalf("world-writable %s passed the check", writable)
	}

	sh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
	if st, err := lstatFile(sh); err != nil || st.UID != 0 {
		t.Skipf("%s is not root-owned on this host", sh)
	}
	if err := checkRootOnlyChain(sh, lstatFile); err != nil {
		t.Fatalf("system shell %s failed the check: %v", sh, err)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./internal/pathtrust ./cmd/runner -count=1 -run 'TestProblem|TestCheckRootOnly'`
Expected: FAIL，`undefined: Problem`、`undefined: fileStat` 等編譯錯誤

- [ ] **Step 3: 實作**

`internal/pathtrust/pathtrust.go`：

```go
// Package pathtrust decides whether only root can change a file.
package pathtrust

import (
	"fmt"
	"io/fs"
)

// Problem explains why someone other than root could change a file with this
// owner and mode, or returns "" when only root can. Group write is never
// accepted: a POSIX ACL mask shows up in the group bits, and the root group
// can have other members.
func Problem(uid uint32, mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "is a symlink"
	case uid != 0:
		return fmt.Sprintf("is owned by uid %d, not root", uid)
	case mode.Perm()&0o020 != 0:
		return "is writable by its group"
	case mode.Perm()&0o002 != 0:
		return "is writable by others"
	}
	return ""
}
```

`cmd/runner/trusted_path.go`：

```go
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/ysya/runscaler/internal/pathtrust"
)

// fileStat is the ownership data the path checks need.
type fileStat struct {
	UID  uint32
	Mode fs.FileMode
}

type statFunc func(path string) (fileStat, error)

// lstatFile reads ownership without following a final symlink.
func lstatFile(path string) (fileStat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileStat{}, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStat{}, fmt.Errorf("stat %s: ownership unavailable on this platform", path)
	}
	return fileStat{UID: sys.Uid, Mode: info.Mode()}, nil
}

// untrustedPathError names a path someone other than root could change.
type untrustedPathError struct {
	Path   string
	Reason string
}

func (e *untrustedPathError) Error() string { return e.Path + " " + e.Reason }

// checkRootOnly requires that only root can change path itself.
func checkRootOnly(path string, stat statFunc) error {
	st, err := stat(path)
	if err != nil {
		return err
	}
	if reason := pathtrust.Problem(st.UID, st.Mode); reason != "" {
		return &untrustedPathError{Path: path, Reason: reason}
	}
	return nil
}

// checkRootOnlyChain applies checkRootOnly to an absolute path and every
// ancestor: whoever can write a parent directory can replace the child.
func checkRootOnlyChain(path string, stat statFunc) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if err := checkRootOnly(p, stat); err != nil {
			return err
		}
		if p == "/" {
			return nil
		}
	}
}
```

- [ ] **Step 4: 確認測試通過**

Run: `go test ./internal/pathtrust ./cmd/runner -count=1 -run 'TestProblem|TestCheckRootOnly' -v`
Expected: 全部 PASS（`/bin/sh` 非 root 擁有的主機上該段 SKIP）

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add internal/pathtrust cmd/runner/trusted_path.go cmd/runner/trusted_path_test.go
git commit -m "feat(runner): add root-only path trust checks"
```

---

### Task 3: lock 讓 root 與一般使用者能輪流持有

**Files:**
- Modify: `internal/lock/lock.go`（`Acquire`，新增 `sysOps`、`openLockFile`）
- Modify: `internal/lock/lock_test.go`

**Interfaces:**
- Consumes: 無
- Produces: `lock.Acquire(path string, info Info) (func(), error)` 簽名不變；新增錯誤情境「hard links」

- [ ] **Step 1: 寫失敗測試**

`internal/lock/lock_test.go` 新增（import 補 `"strings"`、`"golang.org/x/sys/unix"`）：

```go
func withSys(t *testing.T, replace func(*sysOps)) {
	t.Helper()
	saved := sys
	replace(&sys)
	t.Cleanup(func() { sys = saved })
}

func TestAcquireRejectsHardLinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "precious")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner.lock")
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(path, Info{PID: 1}); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("Acquire() on hard link = %v, want hard link error", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" || info.Mode().Perm() != 0o600 {
		t.Fatalf("hard link target changed: %q mode %o", data, info.Mode().Perm())
	}
}

func TestAcquireLeavesForeignOwnedLockModeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	chmods := 0
	withSys(t, func(s *sysOps) {
		realFstat := s.fstat
		s.fstat = func(fd int, st *unix.Stat_t) error {
			err := realFstat(fd, st)
			st.Uid = uint32(os.Geteuid() + 1)
			return err
		}
		s.fchmod = func(int, uint32) error {
			chmods++
			return nil
		}
	})

	release, err := Acquire(path, Info{PID: 1})
	if err != nil {
		t.Fatalf("Acquire() on a lock owned by someone else = %v", err)
	}
	release()
	if chmods != 0 {
		t.Fatalf("fchmod called %d times on a lock this process does not own", chmods)
	}
}

func TestAcquireCreatesMissingLockExclusively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	var modes []int
	withSys(t, func(s *sysOps) {
		realOpen := s.open
		s.open = func(p string, mode int, perm uint32) (int, error) {
			modes = append(modes, mode)
			return realOpen(p, mode, perm)
		}
	})

	release, err := Acquire(path, Info{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(modes) != 2 || modes[0]&unix.O_CREAT != 0 || modes[1]&(unix.O_CREAT|unix.O_EXCL) != unix.O_CREAT|unix.O_EXCL {
		t.Fatalf("open modes = %#v, want plain open then O_CREAT|O_EXCL", modes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o666 {
		t.Fatalf("created lock mode = %o, want 666", info.Mode().Perm())
	}
}

func TestAcquireRetriesWhenAnotherProcessCreatesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.lock")
	var modes []int
	withSys(t, func(s *sysOps) {
		realOpen := s.open
		s.open = func(p string, mode int, perm uint32) (int, error) {
			modes = append(modes, mode)
			switch len(modes) {
			case 1:
				return -1, unix.ENOENT
			case 2:
				// Another runner wins the create race.
				if err := os.WriteFile(p, nil, 0o666); err != nil {
					return -1, err
				}
				return -1, unix.EEXIST
			}
			return realOpen(p, mode, perm)
		}
	})

	release, err := Acquire(path, Info{PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(modes) != 3 || modes[2]&unix.O_CREAT != 0 {
		t.Fatalf("open modes = %#v, want a plain reopen after EEXIST", modes)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./internal/lock -count=1`
Expected: FAIL，`undefined: sysOps`、`undefined: sys`

- [ ] **Step 3: 實作**

`internal/lock/lock.go` 在 `ErrAlreadyRunning` 之後加入：

```go
// sysOps are the system calls Acquire depends on, replaceable in tests.
type sysOps struct {
	open    func(path string, mode int, perm uint32) (int, error)
	fstat   func(fd int, stat *unix.Stat_t) error
	fchmod  func(fd int, mode uint32) error
	geteuid func() int
}

var sys = sysOps{open: unix.Open, fstat: unix.Fstat, fchmod: unix.Fchmod, geteuid: os.Geteuid}

// openLockFile opens an existing lock without O_CREAT and creates a missing
// one with O_EXCL. With fs.protected_regular enabled, the kernel refuses an
// O_CREAT open of a file in /tmp owned by someone else, even for root, so a
// plain O_CREAT open would lock root out of a file a user created first.
func openLockFile(path string) (int, error) {
	const flags = unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	for attempt := 0; attempt < 3; attempt++ {
		fd, err := sys.open(path, flags, 0)
		if !errors.Is(err, unix.ENOENT) {
			return fd, err
		}
		fd, err = sys.open(path, flags|unix.O_CREAT|unix.O_EXCL, 0o666)
		if !errors.Is(err, unix.EEXIST) {
			return fd, err
		}
	}
	return -1, fmt.Errorf("%s was repeatedly created and removed while opening it", path)
}
```

`Acquire` 開頭到 `Flock` 之前改為：

```go
func Acquire(path string, info Info) (release func(), err error) {
	fd, err := openLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("open runner lock %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open runner lock %s: invalid file descriptor", path)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = f.Close()
		}
	}()

	var stat unix.Stat_t
	if err := sys.fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("inspect runner lock %s: %w", path, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("runner lock %s is not a regular file", path)
	}
	// A hard link would let the lock chmod and truncate another file.
	if uint64(stat.Nlink) != 1 {
		return nil, fmt.Errorf("runner lock %s has %d hard links; remove it once no runner is running", path, stat.Nlink)
	}
	// Root-run and user-run instances must be able to contend on the same file.
	// Only its owner may chmod it; everyone else can use it as it is.
	if int(stat.Uid) == sys.geteuid() {
		if err := sys.fchmod(fd, 0o666); err != nil {
			return nil, fmt.Errorf("set runner lock permissions %s: %w", path, err)
		}
	}
```

其餘（`Flock`、`writeInfo`、release）不變。`Acquire` 的註解最後補一句：`The lock guards against accidentally starting a second runner; it is not a security boundary against local users.`

- [ ] **Step 4: 確認測試通過**

Run: `go test ./internal/lock -count=1 -v`
Expected: 既有 3 個與新增 4 個測試全部 PASS

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add internal/lock
git commit -m "fix(lock): let root and users take turns holding the run lock"
```

---

### Task 4: log 檔改以目錄 fd 開檔與輪替，停用 log 檔時不再 panic

**Files:**
- Modify: `internal/config/logfile.go`
- Modify: `internal/config/config.go:779-845`（logger 建構函式）
- Modify: `internal/config/logfile_test.go`
- Modify: `cmd/runner/main.go`（兩個 logger 呼叫點與 `OpenLogFile` 呼叫）
- Create: `cmd/runner/logpath.go`（先放 `logWriter`）
- Create: `cmd/runner/logpath_test.go`

**Interfaces:**
- Consumes: `pathtrust.Problem`（Task 2）
- Produces:
  - `func config.OpenLogFile(path string, dirPerm fs.FileMode) (*config.LogFileWriter, error)`
  - `func config.OpenRootLogFile(path, trustedRoot string, dirPerm fs.FileMode) (*config.LogFileWriter, error)`
  - `func config.NewLoggerWithWriter(level, format string, console *os.File, file io.Writer) *slog.Logger`
  - `func config.NewScaleSetLoggerWithWriter(level, format, name string, index int, console *os.File, file io.Writer) *slog.Logger`
  - 套件內：`func openLogFile(path string, dirPerm fs.FileMode, maxSize int64) (*LogFileWriter, error)`、`func openTrustedLogFile(path, trustedRoot string, dirPerm fs.FileMode, maxSize int64, problem func(*unix.Stat_t) string) (*LogFileWriter, error)`
  - `func logWriter(f *config.LogFileWriter) io.Writer`（`cmd/runner`）

- [ ] **Step 1: 寫失敗測試（config 套件）**

`internal/config/logfile_test.go` 整份改為（保留 `setColorEnv`）：

```go
package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLogFileWriterRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	w, err := openLogFile(path, 0o700, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "new\n" || string(backup) != "12345678" {
		t.Fatalf("current=%q backup=%q", current, backup)
	}
}

func TestLoggerWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	w, err := OpenLogFile(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	NewLoggerWithWriter("info", "json", os.Stdout, w).Info("hello-log")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello-log") {
		t.Fatalf("log contents = %q", data)
	}
}

func TestLoggerColorsConsoleButNotLogFile(t *testing.T) {
	setColorEnv(t, "1") // stands in for a color terminal
	console, read := pipeConsole(t)
	var file bytes.Buffer

	NewScaleSetLoggerWithWriter("info", "text", "linux", 0, console, &file).Warn("disk low", "free", "2GB")

	if got := read(); !strings.Contains(got, "\x1b[") {
		t.Errorf("console lost its styling: %q", got)
	}
	if got := file.String(); strings.Contains(got, "\x1b") || !strings.Contains(got, " WARN linux: disk low free=2GB\n") {
		t.Errorf("log file = %q, want plain text", got)
	}
}

func TestLoggerPlainWhenConsoleIsNotTerminal(t *testing.T) {
	setColorEnv(t, "")
	console, read := pipeConsole(t)
	var file bytes.Buffer

	NewScaleSetLoggerWithWriter("info", "text", "linux", 0, console, &file).Warn("disk low", "free", "2GB")

	if got := read(); strings.Contains(got, "\x1b") || !strings.Contains(got, " WARN linux: disk low free=2GB\n") {
		t.Errorf("console = %q, want plain text", got)
	}
}

func TestOpenLogFileRefusesSymlinkedLogFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runner.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if w, err := OpenLogFile(path, 0o700); err == nil {
		_ = w.Close()
		t.Fatal("OpenLogFile followed a symlink")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep" {
		t.Fatalf("symlink target changed to %q", data)
	}
}

func TestOpenLogFileCreatesDirectoryWithGivenMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "runner")
	w, err := OpenLogFile(filepath.Join(dir, "runner.log"), 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("log directory mode = %o, want 700", info.Mode().Perm())
	}
}

// userOnlyProblem stands in for the root check so these tests run
// unprivileged: a directory must belong to this process and be writable by
// nobody else.
func userOnlyProblem(st *unix.Stat_t) string {
	if int(st.Uid) != os.Geteuid() {
		return "is owned by someone else"
	}
	if st.Mode&0o022 != 0 {
		return "is writable by group or others"
	}
	return ""
}

// trustedTempRoot resolves symlinks in the temp path (macOS /var is one),
// since the trusted walk refuses every symlinked component.
func trustedTempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestOpenTrustedLogFileCreatesMissingDirectories(t *testing.T) {
	root := trustedTempRoot(t)
	dir := filepath.Join(root, "runner", "nested")
	w, err := openTrustedLogFile(filepath.Join(dir, "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "runner.log")); err != nil || string(data) != "hello\n" {
		t.Fatalf("log = %q, %v", data, err)
	}
}

func TestOpenTrustedLogFileRejectsSymlinkedDirectory(t *testing.T) {
	root := trustedTempRoot(t)
	elsewhere := trustedTempRoot(t)
	if err := os.Symlink(elsewhere, filepath.Join(root, "runner")); err != nil {
		t.Fatal(err)
	}
	if w, err := openTrustedLogFile(filepath.Join(root, "runner", "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem); err == nil {
		_ = w.Close()
		t.Fatal("trusted open followed a symlinked directory")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("symlink target received files: %v", entries)
	}
}

func TestOpenTrustedLogFileRejectsUnsafeDirectory(t *testing.T) {
	root := trustedTempRoot(t)
	dir := filepath.Join(root, "runner")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	w, err := openTrustedLogFile(filepath.Join(dir, "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem)
	if err == nil {
		_ = w.Close()
		t.Fatal("trusted open accepted a world-writable directory")
	}
	if !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenTrustedLogFileRequiresADirectoryBelowTheRoot(t *testing.T) {
	root := trustedTempRoot(t)
	if w, err := openTrustedLogFile(filepath.Join(root, "runner.log"), root, 0o755, DefaultLogMaxSize, userOnlyProblem); err == nil {
		_ = w.Close()
		t.Fatal("log directly in the trusted root was accepted")
	}
}

func TestTrustedLogWriterRotatesInsideTheOriginalDirectory(t *testing.T) {
	root := trustedTempRoot(t)
	dir := filepath.Join(root, "runner")
	w, err := openTrustedLogFile(filepath.Join(dir, "runner.log"), root, 0o755, 10, userOnlyProblem)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	// Replace the directory's path with a symlink to somewhere else.
	moved := filepath.Join(root, "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	decoy := trustedTempRoot(t)
	if err := os.Symlink(decoy, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("rotated\n")); err != nil { // 8+8 bytes exceeds 10 and rotates
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(filepath.Join(moved, "runner.log"))
	backup, _ := os.ReadFile(filepath.Join(moved, "runner.log.1"))
	if string(current) != "rotated\n" || string(backup) != "12345678" {
		t.Fatalf("original directory current=%q backup=%q", current, backup)
	}
	if entries, _ := os.ReadDir(decoy); len(entries) != 0 {
		t.Fatalf("rotation followed the swapped path into %s: %v", decoy, entries)
	}
}

// pipeConsole returns a pipe to use as a logger console, which is never a
// terminal, and a function that closes it and yields what was written.
func pipeConsole(t *testing.T) (*os.File, func() string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return w, func() string {
		_ = w.Close()
		out, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
}

// setColorEnv pins the variables that decide whether colorprofile styles
// output, so the developer's terminal cannot leak into a test.
func setColorEnv(t *testing.T, cliColorForce string) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "")
	t.Setenv("CLICOLOR_FORCE", cliColorForce)
	t.Setenv("TTY_FORCE", "")
}
```

- [ ] **Step 2: 寫失敗測試（cmd/runner 的 panic 回歸）**

`cmd/runner/logpath_test.go`：

```go
package main

import (
	"os"
	"testing"

	"github.com/ysya/runscaler/internal/config"
)

func TestLogWriterKeepsDisabledFileLoggingNil(t *testing.T) {
	var disabled *config.LogFileWriter
	if logWriter(disabled) != nil {
		t.Fatal("a nil *LogFileWriter became a non-nil io.Writer")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	// Before the fix every log line panicked on the typed-nil file writer.
	config.NewLoggerWithWriter("info", "text", w, logWriter(disabled)).Info("still logs")
}
```

- [ ] **Step 3: 確認測試失敗**

Run: `go test ./internal/config ./cmd/runner -count=1`
Expected: FAIL，`undefined: openTrustedLogFile`、參數數量不符、`undefined: logWriter` 等編譯錯誤

- [ ] **Step 4: 實作 `internal/config/logfile.go`**

整份改為：

```go
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/colorprofile"
	"golang.org/x/sys/unix"

	"github.com/ysya/runscaler/internal/pathtrust"
)

const (
	DefaultLogFileName = "runner.log"
	DefaultLogMaxSize  = int64(10 * 1024 * 1024)
)

// LogFileWriter is a concurrency-safe rotating writer shared by every logger
// in a process. Rotation keeps one backup at name + ".1". Every open, rename
// and unlink is relative to the directory opened at start, so replacing that
// directory's path afterwards cannot redirect the log.
type LogFileWriter struct {
	mu      sync.Mutex
	dir     *os.File
	name    string
	maxSize int64
	file    *os.File
	size    int64
}

// OpenLogFile opens path for appending, creating its directory with dirPerm.
func OpenLogFile(path string, dirPerm fs.FileMode) (*LogFileWriter, error) {
	return openLogFile(path, dirPerm, DefaultLogMaxSize)
}

func openLogFile(path string, dirPerm fs.FileMode, maxSize int64) (*LogFileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return newLogFileWriter(dir, filepath.Base(path), maxSize)
}

// OpenRootLogFile opens path for a root writer. Every component from / is
// opened without following symlinks; directories below trustedRoot are
// created with dirPerm when missing and must be changeable only by root.
// trustedRoot itself may be group-writable (/var/log is on some
// distributions): once the writer holds its directory, renaming anything
// above it cannot redirect writes or rotation.
func OpenRootLogFile(path, trustedRoot string, dirPerm fs.FileMode) (*LogFileWriter, error) {
	return openTrustedLogFile(path, trustedRoot, dirPerm, DefaultLogMaxSize, rootOnlyProblem)
}

func rootOnlyProblem(st *unix.Stat_t) string {
	return pathtrust.Problem(st.Uid, fs.FileMode(st.Mode&0o777))
}

func openTrustedLogFile(path, trustedRoot string, dirPerm fs.FileMode, maxSize int64, problem func(*unix.Stat_t) string) (*LogFileWriter, error) {
	dir, err := openTrustedDir(filepath.Dir(path), trustedRoot, dirPerm, problem)
	if err != nil {
		return nil, err
	}
	return newLogFileWriter(dir, filepath.Base(path), maxSize)
}

// openTrustedDir walks from / to dirPath opening each component with
// O_NOFOLLOW, creating and vetting the components below trustedRoot.
func openTrustedDir(dirPath, trustedRoot string, dirPerm fs.FileMode, problem func(*unix.Stat_t) string) (*os.File, error) {
	dirPath = filepath.Clean(dirPath)
	trustedRoot = filepath.Clean(trustedRoot)
	rel, err := filepath.Rel(trustedRoot, dirPath)
	if !filepath.IsAbs(dirPath) || err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("log directory %s is not below %s", dirPath, trustedRoot)
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /: %w", err)
	}
	current := "/"
	for _, name := range strings.Split(strings.TrimPrefix(dirPath, "/"), "/") {
		next := filepath.Join(current, name)
		below := strings.HasPrefix(next, trustedRoot+"/")
		child, err := unix.Openat(fd, name, flags, 0)
		if errors.Is(err, unix.ENOENT) && below {
			if mkErr := unix.Mkdirat(fd, name, uint32(dirPerm.Perm())); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("create %s: %w", next, mkErr)
			}
			child, err = unix.Openat(fd, name, flags, 0)
		}
		_ = unix.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("open %s without following symlinks: %w", next, err)
		}
		fd = child
		if below {
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("inspect %s: %w", next, err)
			}
			if reason := problem(&st); reason != "" {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("log directory %s %s", next, reason)
			}
		}
		current = next
	}
	return os.NewFile(uintptr(fd), dirPath), nil
}

func newLogFileWriter(dir *os.File, name string, maxSize int64) (*LogFileWriter, error) {
	w := &LogFileWriter{dir: dir, name: name, maxSize: maxSize}
	f, err := w.openAt(unix.O_APPEND)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = dir.Close()
		return nil, err
	}
	w.file, w.size = f, info.Size()
	return w, nil
}

// openAt opens the log inside the writer's directory. O_NOFOLLOW refuses a
// symlink planted at the log's own name.
func (w *LogFileWriter) openAt(mode int) (*os.File, error) {
	path := filepath.Join(w.dir.Name(), w.name)
	fd, err := unix.Openat(int(w.dir.Fd()), w.name, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|mode, 0o640)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func (w *LogFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.maxSize > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, fmt.Errorf("rotate log: %w", err)
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *LogFileWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	dirfd := int(w.dir.Fd())
	backup := w.name + ".1"
	_ = unix.Unlinkat(dirfd, backup, 0)
	if err := unix.Renameat(dirfd, w.name, dirfd, backup); err != nil && !errors.Is(err, unix.ENOENT) {
		w.file, _ = w.openAt(unix.O_APPEND)
		return err
	}
	f, err := w.openAt(unix.O_TRUNC)
	if err != nil {
		_ = unix.Renameat(dirfd, backup, dirfd, w.name)
		w.file, _ = w.openAt(unix.O_APPEND)
		return err
	}
	w.file, w.size = f, 0
	return nil
}

func (w *LogFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var errs []error
	if w.file != nil {
		errs = append(errs, w.file.Close())
		w.file = nil
	}
	if w.dir != nil {
		errs = append(errs, w.dir.Close())
		w.dir = nil
	}
	return errors.Join(errs...)
}

// outputWriter tees log output to the console and, when enabled, the log
// file. The file copy is stripped of styling so runner.log stays plain text.
func outputWriter(console *os.File, file io.Writer) io.Writer {
	if file == nil {
		return console
	}
	return io.MultiWriter(console, &colorprofile.Writer{Forward: file, Profile: colorprofile.NoTTY})
}

// consoleColorProfile picks log styling from the console itself: color on an
// interactive terminal, plain text on the pipes, sockets and files that
// systemd, launchd, docker run without -t and CI provide. charmlog would
// otherwise probe its writer, and the tee outputWriter builds for a log file
// is never a terminal. NO_COLOR, CLICOLOR_FORCE and TERM=dumb are honored.
func consoleColorProfile(console *os.File) colorprofile.Profile {
	return colorprofile.Detect(console, os.Environ())
}
```

注意：`w.file, _ = w.openAt(...)` 失敗時 `w.file` 為 nil，下一次 `Write` 回傳 `os.ErrClosed`，與原本行為相同。

- [ ] **Step 5: 實作 logger 建構函式**

`internal/config/config.go`：

```go
func NewLogger(level, format string) *slog.Logger {
	return NewLoggerWithWriter(level, format, os.Stdout, nil)
}

// NewLoggerWithWriter creates a process logger writing to console and, when
// file is non-nil, teeing to it. The writer may be shared with scale-set loggers.
func NewLoggerWithWriter(level, format string, console *os.File, file io.Writer) *slog.Logger {
```

函式本體內 `outputWriter(file)` 改 `outputWriter(console, file)`、`stdoutColorProfile()` 改 `consoleColorProfile(console)`。`NewScaleSetLogger` 改傳 `os.Stdout, nil`；`NewScaleSetLoggerWithWriter(level, format string, name string, index int, console *os.File, file io.Writer)` 同樣替換。

- [ ] **Step 6: 實作 cmd/runner**

`cmd/runner/logpath.go`：

```go
package main

import (
	"io"

	"github.com/ysya/runscaler/internal/config"
)

// logWriter keeps a nil *LogFileWriter from becoming a non-nil io.Writer,
// which made every log line panic when file logging was off.
func logWriter(f *config.LogFileWriter) io.Writer {
	if f == nil {
		return nil
	}
	return f
}
```

`cmd/runner/main.go` 暫時維持既有路徑邏輯，只修改：
- `config.OpenLogFile(path)` → `config.OpenLogFile(path, 0o755)`
- `config.NewLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, logFile)` → `config.NewLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, os.Stdout, logWriter(logFile))`
- `config.NewScaleSetLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, ss.ScaleSetName, i, logFile)` → `config.NewScaleSetLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, ss.ScaleSetName, i, os.Stdout, logWriter(logFile))`

- [ ] **Step 7: 確認測試通過**

Run: `go test ./internal/config ./cmd/runner -count=1`
Expected: PASS

- [ ] **Step 8: 以實際 binary 確認不再 panic**

```bash
tmp=$(mktemp -d) && printf 'log-level = "info"\nlog-file = ""\nbogus-key = true\n' > "$tmp/config.toml"
go build -o "$tmp/runner" ./cmd/runner
"$tmp/runner" run --config "$tmp/config.toml" </dev/null >"$tmp/out.txt" 2>&1
grep -c panic "$tmp/out.txt"; grep -c 'unknown config key' "$tmp/out.txt"
```

Expected: 第一個數字 `0`、第二個 `1`（程式越過 logger 並印出 WARN）

- [ ] **Step 9: 全綠檢查並提交**

```bash
git add internal/config cmd/runner/logpath.go cmd/runner/logpath_test.go cmd/runner/main.go
git commit -m "fix(log): keep log writes inside the opened directory and stop panicking without a file"
```

---

### Task 5: log 路徑決策與開檔 fallback

**Files:**
- Modify: `cmd/runner/logpath.go`
- Modify: `cmd/runner/logpath_test.go`
- Modify: `cmd/runner/loadconfig.go`（移除 `resolveLogFilePath`）
- Modify: `cmd/runner/main.go`（`runManager` 的 log 設定）
- Modify: `cmd/runner/cmd_logs.go`（改用 `resolveLogFile`，import 補 `layout`）

**Interfaces:**
- Consumes: `layout.CurrentIdentity`、`layout.For`、`layout.SystemLogsDirectory`、`layout.SystemLogRoot`、`Identity.DirPerm`（Task 1）；`config.OpenLogFile`、`config.OpenRootLogFile`（Task 4）；`sameFilePath`（`migrate_config.go`）
- Produces:
  - `type logFileDecision struct { Path, Fallback string; Enabled bool; Warning string }`
  - `func resolveLogFile(cfg config.Config, id layout.Identity) logFileDecision`
  - `var openLogFileFor func(id layout.Identity, path string) (*config.LogFileWriter, error)`
  - `func openRunLog(id layout.Identity, d logFileDecision) (*config.LogFileWriter, string, []string)`
  - `func oldDefaultLogFile(cfg config.Config, usedConfig, current string) string`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/logpath_test.go` 追加（import 補 `"errors"`、`"path/filepath"`、`"strings"`、`"github.com/ysya/runscaler/internal/layout"`）：

```go
func TestResolveLogFile(t *testing.T) {
	str := func(s string) *string { return &s }
	user := layout.Identity{GOOS: "linux", Home: "/home/ada"}
	homeless := layout.Identity{GOOS: "linux"}
	root := layout.Identity{GOOS: "linux", Root: true}
	tests := []struct {
		name        string
		id          layout.Identity
		logFile     *string
		want        logFileDecision
		wantWarning string
	}{
		{name: "user default", id: user, want: logFileDecision{Path: "/home/ada/.local/state/runner/runner.log", Enabled: true}},
		{name: "user explicit is kept anywhere", id: user, logFile: str("/home/ada/runner/runner.log"), want: logFileDecision{Path: "/home/ada/runner/runner.log", Enabled: true}},
		{name: "disabled", id: user, logFile: str("")},
		{name: "user without a default location", id: homeless, wantWarning: "no default log location"},
		{name: "user without home keeps an explicit path", id: homeless, logFile: str("/srv/runner.log"), want: logFileDecision{Path: "/srv/runner.log", Enabled: true}},
		{name: "root default", id: root, want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}},
		{name: "root explicit under a var log directory keeps a fallback", id: root, logFile: str("/var/log/custom/runner.log"), want: logFileDecision{Path: "/var/log/custom/runner.log", Fallback: "/var/log/runner/runner.log", Enabled: true}},
		{name: "root explicit outside var log", id: root, logFile: str("/home/ada/runner/runner.log"), want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}, wantWarning: "directory under /var/log"},
		{name: "root explicit directly in var log", id: root, logFile: str("/var/log/runner.log"), want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}, wantWarning: "directory under /var/log"},
		{name: "root explicit with characters systemd would need escaped", id: root, logFile: str("/var/log/has space/runner.log"), want: logFileDecision{Path: "/var/log/runner/runner.log", Enabled: true}, wantWarning: "directory under /var/log"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveLogFile(config.Config{LogFile: tt.logFile}, tt.id)
			warning := got.Warning
			got.Warning = ""
			if got != tt.want {
				t.Errorf("resolveLogFile() = %+v, want %+v", got, tt.want)
			}
			if (tt.wantWarning == "") != (warning == "") || !strings.Contains(warning, tt.wantWarning) {
				t.Errorf("warning = %q, want it to contain %q", warning, tt.wantWarning)
			}
		})
	}
}

func TestOpenRunLogFallsBackOnce(t *testing.T) {
	good := filepath.Join(t.TempDir(), "runner.log")
	saved := openLogFileFor
	t.Cleanup(func() { openLogFileFor = saved })
	var tried []string
	openLogFileFor = func(_ layout.Identity, path string) (*config.LogFileWriter, error) {
		tried = append(tried, path)
		if path == good {
			return config.OpenLogFile(path, 0o700)
		}
		return nil, errors.New("unsafe directory")
	}

	w, path, warnings := openRunLog(layout.Identity{Root: true}, logFileDecision{Path: "/var/log/custom/runner.log", Fallback: good, Enabled: true})
	if w == nil || path != good {
		t.Fatalf("openRunLog() = %v, %q; want the fallback", w, path)
	}
	_ = w.Close()
	if strings.Join(tried, ",") != "/var/log/custom/runner.log,"+good || len(warnings) != 2 {
		t.Fatalf("tried %v, warnings %q", tried, warnings)
	}

	tried = nil
	w, path, warnings = openRunLog(layout.Identity{}, logFileDecision{Path: "/nowhere/runner.log", Enabled: true})
	if w != nil || path != "" || len(tried) != 1 || len(warnings) != 1 {
		t.Fatalf("without fallback: w=%v path=%q tried=%v warnings=%q", w, path, tried, warnings)
	}
}

func TestOldDefaultLogFile(t *testing.T) {
	dir := t.TempDir()
	usedConfig := filepath.Join(dir, "config.toml")
	old := filepath.Join(dir, "runner.log")
	current := filepath.Join(t.TempDir(), "runner.log")
	explicit := old

	if got := oldDefaultLogFile(config.Config{}, usedConfig, current); got != "" {
		t.Fatalf("missing old log reported as %q", got)
	}
	if err := os.WriteFile(old, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := oldDefaultLogFile(config.Config{}, usedConfig, current); got != old {
		t.Fatalf("oldDefaultLogFile() = %q, want %q", got, old)
	}
	if got := oldDefaultLogFile(config.Config{}, usedConfig, old); got != "" {
		t.Fatalf("current log reported as old: %q", got)
	}
	if got := oldDefaultLogFile(config.Config{LogFile: &explicit}, usedConfig, current); got != "" {
		t.Fatalf("explicit log-file reported as old default: %q", got)
	}
	if got := oldDefaultLogFile(config.Config{}, "", current); got != "" {
		t.Fatalf("flags-only run reported an old log: %q", got)
	}
	if got := oldDefaultLogFile(config.Config{}, usedConfig, ""); got != "" {
		t.Fatalf("run without a log file reported an old log: %q", got)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestResolveLogFile|TestOpenRunLog|TestOldDefaultLogFile'`
Expected: FAIL，`undefined: resolveLogFile` 等編譯錯誤

- [ ] **Step 3: 實作**

`cmd/runner/logpath.go` 追加（import 補 `"fmt"`、`"os"`、`"path/filepath"`、`"github.com/ysya/runscaler/internal/layout"`）：

```go
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
```

`cmd/runner/loadconfig.go`：刪除 `resolveLogFilePath`（`filepath` 仍由 `isLegacyConfigFile` 使用，保留）。

`cmd/runner/main.go` 的 `runManager` log 區塊（`var logFile *config.LogFileWriter` 起到 `logger := ...` 為止）改為：

```go
	id := layout.CurrentIdentity()
	logFile, logPath, logWarnings := openRunLog(id, resolveLogFile(cfg, id))
	for _, w := range logWarnings {
		fmt.Fprintln(os.Stderr, "Warning: "+w)
	}
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}
	logger := config.NewLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, os.Stdout, logWriter(logFile))
	if old := oldDefaultLogFile(cfg, viper.ConfigFileUsed(), logPath); old != "" {
		logger.Info("Log file location changed; the old file is left in place",
			slog.String("path", logPath), slog.String("old", old))
	}
```

import 補 `"github.com/ysya/runscaler/internal/layout"`。

`cmd/runner/cmd_logs.go` 的 `runLogs` 前段改為：

```go
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	id := layout.CurrentIdentity()
	decision := resolveLogFile(cfg, id)
	if !decision.Enabled {
		if decision.Warning != "" {
			return errors.New(decision.Warning)
		}
		return fmt.Errorf("runner file logging is disabled by log-file = \"\"")
	}
	path := decision.Path
```

import 補 `"github.com/ysya/runscaler/internal/layout"`（`errors`、`fmt` 已存在）。

- [ ] **Step 4: 確認測試通過**

Run: `go test ./cmd/runner -count=1`
Expected: PASS

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add cmd/runner/logpath.go cmd/runner/logpath_test.go cmd/runner/loadconfig.go cmd/runner/main.go cmd/runner/cmd_logs.go
git commit -m "feat(log): write logs to per-identity default locations"
```

---

### Task 6: 過舊 service 提醒與單一重裝指令

**Files:**
- Create: `cmd/runner/service_env.go`
- Create: `cmd/runner/service_env_test.go`
- Modify: `cmd/runner/legacy.go`（新增 `legacyLaunchdLabel`）
- Modify: `cmd/runner/main.go`（刪除 `logServiceDrainTimeoutReminder`；`explainLockError` 新簽名；`runManager` 改呼叫 `warnOutdatedService`；`startManager` 傳入修正指令）
- Modify: `cmd/runner/main_test.go`（刪除 reminder 測試；更新 `TestExplainLockError`）

**Interfaces:**
- Consumes: `serviceStopTimeout(*time.Duration) time.Duration`、`launchdLabel`（`cmd_service.go`）；`shellQuotePath(string) string`（`migrate_config.go`）
- Produces:
  - `const serviceVersionEnv = "RUNNER_SERVICE_VERSION"`、`serviceStopTimeoutEnv = "RUNNER_SERVICE_STOP_TIMEOUT"`、`currentServiceVersion = 2`
  - `legacyLaunchdLabel = "com.runscaler.agent"`（`legacy.go` 的 var）
  - `func startedByServiceManager(getenv func(string) string) bool`
  - `func outdatedServiceWarnings(getenv func(string) string, drainTimeout time.Duration) []string`
  - `func serviceReinstallCommand(root bool, configPath, binaryPath string) string`
  - `func currentBinaryPath() string`、`func absConfigPath(path string) string`
  - `func warnOutdatedService(logger *slog.Logger, drainTimeout time.Duration)`
  - `func explainLockError(err error, fix string) error`
  - 測試 helper：`func envFrom(values map[string]string) func(string) string`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/service_env_test.go`：

```go
package main

import (
	"strings"
	"testing"
	"time"
)

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestStartedByServiceManager(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "systemd", env: map[string]string{"INVOCATION_ID": "abc"}, want: true},
		{name: "current launchd agent", env: map[string]string{"XPC_SERVICE_NAME": "io.github.ysya.runner"}, want: true},
		{name: "legacy launchd agent", env: map[string]string{"XPC_SERVICE_NAME": "com.runscaler.agent"}, want: true},
		{name: "macOS terminal", env: map[string]string{"XPC_SERVICE_NAME": "0"}},
		{name: "plain shell"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := startedByServiceManager(envFrom(tt.env)); got != tt.want {
				t.Errorf("startedByServiceManager() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOutdatedServiceWarnings(t *testing.T) {
	drain := 2 * time.Hour
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{name: "not a service", env: map[string]string{serviceVersionEnv: "1"}},
		{name: "template without version", env: map[string]string{"INVOCATION_ID": "abc"}, want: []string{"predates"}},
		{name: "unparseable version", env: map[string]string{"INVOCATION_ID": "abc", serviceVersionEnv: "two"}, want: []string{"predates"}},
		{name: "current and long enough", env: map[string]string{"INVOCATION_ID": "abc", serviceVersionEnv: "2", serviceStopTimeoutEnv: "7260"}},
		{name: "current but stop timeout too short", env: map[string]string{"XPC_SERVICE_NAME": "io.github.ysya.runner", serviceVersionEnv: "2", serviceStopTimeoutEnv: "3660"}, want: []string{"shorter than drain-timeout"}},
		{name: "legacy label without version", env: map[string]string{"XPC_SERVICE_NAME": "com.runscaler.agent"}, want: []string{"predates"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := outdatedServiceWarnings(envFrom(tt.env), drain)
			if len(got) != len(tt.want) {
				t.Fatalf("warnings = %q, want %d matching %q", got, len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("warning %d = %q, want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}

func TestServiceReinstallCommand(t *testing.T) {
	got := serviceReinstallCommand(true, "/home/ada/runner/config.toml", "/opt/my runner/runner")
	want := "sudo '/opt/my runner/runner' service install --user=false --force --config-path '/home/ada/runner/config.toml' --binary-path '/opt/my runner/runner'"
	if got != want {
		t.Errorf("root command = %q\nwant %q", got, want)
	}
	got = serviceReinstallCommand(false, "", "/Users/ada/.local/bin/runner")
	want = "'/Users/ada/.local/bin/runner' service install --user --force --binary-path '/Users/ada/.local/bin/runner'"
	if got != want {
		t.Errorf("user command = %q\nwant %q", got, want)
	}
}
```

`cmd/runner/main_test.go`：刪除 `TestLogServiceDrainTimeoutReminder` 與 `TestLogServiceDrainTimeoutReminder_SkipsInteractiveOrDisabled`；`TestExplainLockError` 改為：

```go
func TestExplainLockError(t *testing.T) {
	const fix = "sudo /usr/local/bin/runner service install --user=false --force"
	readOnly := explainLockError(fmt.Errorf("open runner lock /tmp/runner.lock: %w", syscall.EROFS), fix)
	if !errors.Is(readOnly, syscall.EROFS) || !strings.Contains(readOnly.Error(), fix) {
		t.Errorf("read-only lock error should keep its cause and include the fix:\n%v", readOnly)
	}

	held := explainLockError(&runnerlock.AlreadyRunningError{Path: "/tmp/runner.lock"}, fix)
	if !errors.Is(held, runnerlock.ErrAlreadyRunning) || !strings.Contains(held.Error(), "Only one runner may run per machine") {
		t.Errorf("held lock error should keep its cause and explain the single-instance rule:\n%v", held)
	}

	other := errors.New("permission denied")
	if got := explainLockError(other, fix); got != other {
		t.Errorf("unrelated lock error = %v, want it unchanged", got)
	}
}
```

刪除測試後若 `log/slog`、`config` import 不再使用，一併移除。

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestStartedByServiceManager|TestOutdatedServiceWarnings|TestServiceReinstallCommand|TestExplainLockError'`
Expected: FAIL，`undefined: startedByServiceManager` 等編譯錯誤

- [ ] **Step 3: 實作**

`cmd/runner/legacy.go` 的 var 區塊加入 `legacyLaunchdLabel = "com.runscaler.agent"`。

`cmd/runner/service_env.go`：

```go
package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const (
	serviceVersionEnv     = "RUNNER_SERVICE_VERSION"
	serviceStopTimeoutEnv = "RUNNER_SERVICE_STOP_TIMEOUT"
	// currentServiceVersion is the template generation that records the two
	// variables above; bump it whenever generated definitions change.
	currentServiceVersion = 2
)

// startedByServiceManager reports whether systemd, or launchd under runner's
// current or legacy label, started this process. A macOS terminal sets
// XPC_SERVICE_NAME=0, so the label must match exactly.
func startedByServiceManager(getenv func(string) string) bool {
	if getenv("INVOCATION_ID") != "" {
		return true
	}
	switch getenv("XPC_SERVICE_NAME") {
	case launchdLabel, legacyLaunchdLabel:
		return true
	}
	return false
}

// outdatedServiceWarnings reads the template generation and stop timeout that
// generated service definitions record. It does not audit hand-edited
// definitions.
func outdatedServiceWarnings(getenv func(string) string, drainTimeout time.Duration) []string {
	if !startedByServiceManager(getenv) {
		return nil
	}
	var warnings []string
	version, err := strconv.Atoi(getenv(serviceVersionEnv))
	if err != nil {
		version = 1
	}
	if version < currentServiceVersion {
		warnings = append(warnings, "Service definition predates this runner's template (log directory, lock access, stop timeout)")
	}
	if seconds, err := strconv.Atoi(getenv(serviceStopTimeoutEnv)); err == nil {
		need := serviceStopTimeout(&drainTimeout)
		if time.Duration(seconds)*time.Second < need {
			warnings = append(warnings, fmt.Sprintf("Service stop timeout %ds is shorter than drain-timeout plus one minute (%s); a restart could kill in-flight jobs", seconds, need))
		}
	}
	return warnings
}

// serviceReinstallCommand regenerates the service in place with the config
// and binary this process actually uses.
func serviceReinstallCommand(root bool, configPath, binaryPath string) string {
	var parts []string
	if root {
		parts = append(parts, "sudo")
	}
	parts = append(parts, shellQuotePath(binaryPath), "service", "install")
	if root {
		parts = append(parts, "--user=false")
	} else {
		parts = append(parts, "--user")
	}
	parts = append(parts, "--force")
	if configPath != "" {
		parts = append(parts, "--config-path", shellQuotePath(configPath))
	}
	parts = append(parts, "--binary-path", shellQuotePath(binaryPath))
	return strings.Join(parts, " ")
}

// currentBinaryPath is the resolved executable, or "runner" when unknown.
func currentBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "runner"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// absConfigPath makes a config path usable from any working directory.
func absConfigPath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// warnOutdatedService logs each reason to regenerate the service together
// with the single command that does it.
func warnOutdatedService(logger *slog.Logger, drainTimeout time.Duration) {
	warnings := outdatedServiceWarnings(os.Getenv, drainTimeout)
	if len(warnings) == 0 {
		return
	}
	fix := serviceReinstallCommand(os.Geteuid() == 0, absConfigPath(viper.ConfigFileUsed()), currentBinaryPath())
	for _, w := range warnings {
		logger.Warn(w, slog.String("fix", fix))
	}
}
```

`cmd/runner/main.go`：
- 刪除 `logServiceDrainTimeoutReminder` 整個函式
- `runManager` 中 `logServiceDrainTimeoutReminder(cfg.EffectiveDrainTimeout(), logger)` 改為 `warnOutdatedService(logger, cfg.EffectiveDrainTimeout())`
- `explainLockError` 改為：

```go
// explainLockError adds the operator action to lock failures that have one.
func explainLockError(err error, fix string) error {
	switch {
	case errors.Is(err, runnerlock.ErrAlreadyRunning):
		return fmt.Errorf("%w\n\n  Only one runner may run per machine.\n  To manage multiple organizations, use multiple [[scaleset]] entries in one config", err)
	case errors.Is(err, syscall.EROFS):
		// A binary update cannot rewrite an installed unit, and system units
		// from older releases keep /tmp read-only under ProtectSystem=strict.
		return fmt.Errorf("%w\n\n  The service sandbox keeps /tmp read-only (a unit from an older release). Regenerate it:\n    %s", err, fix)
	default:
		return err
	}
}
```

- `startManager` 的 `return explainLockError(err)` 改為 `return explainLockError(err, serviceReinstallCommand(os.Geteuid() == 0, absConfigPath(viper.ConfigFileUsed()), currentBinaryPath()))`

- [ ] **Step 4: 確認測試通過**

Run: `go test ./cmd/runner -count=1`
Expected: PASS

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add cmd/runner/service_env.go cmd/runner/service_env_test.go cmd/runner/legacy.go cmd/runner/main.go cmd/runner/main_test.go
git commit -m "feat(service): warn about outdated service definitions with an exact fix"
```

---

### Task 7: `runner run` 的 macOS root 拒絕、usage 靜音與 stdout fallback

**Files:**
- Create: `cmd/runner/platform_guard.go`
- Create: `cmd/runner/platform_guard_test.go`
- Modify: `cmd/runner/logpath.go`（新增 `stdoutIsDevNull`、`logConsole`）
- Modify: `cmd/runner/logpath_test.go`
- Modify: `cmd/runner/main.go`（`startManager`、`runManager`）
- Modify: `cmd/runner/tool_path.go`（抽出 `homebrewPath`）
- Modify: `cmd/runner/tool_path_test.go`

**Interfaces:**
- Consumes: `launchdSystemDir`、`launchdPlistFile`（`cmd_service.go`）、`legacyLaunchdPlist`（`legacy.go`）；`appendMissingDirs`、`homebrewBinDirs`（`tool_path.go`）；`openRunLog`（Task 5）
- Produces:
  - `func refuseDarwinRoot(goos string, euid int, action string, exists func(string) bool) error`
  - `func pathExists(path string) bool`
  - `func stdoutIsDevNull() bool`
  - `func logConsole(stdoutDiscarded, fileOpen bool) *os.File`
  - `func homebrewPath(goos, path string) string`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/platform_guard_test.go`：

```go
package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestRefuseDarwinRoot(t *testing.T) {
	none := func(string) bool { return false }
	if err := refuseDarwinRoot("linux", 0, "run runner", none); err != nil {
		t.Errorf("linux root refused: %v", err)
	}
	if err := refuseDarwinRoot("darwin", 501, "run runner", none); err != nil {
		t.Errorf("macOS user refused: %v", err)
	}
	err := refuseDarwinRoot("darwin", 0, "run runner", none)
	if err == nil || !strings.Contains(err.Error(), "LaunchAgent") || strings.Contains(err.Error(), "Found system service") {
		t.Errorf("macOS root without daemons = %v", err)
	}
	legacy := "/Library/LaunchDaemons/com.runscaler.agent.plist"
	err = refuseDarwinRoot("darwin", 0, "run runner", func(p string) bool { return p == legacy })
	if err == nil || !strings.Contains(err.Error(), legacy) {
		t.Errorf("macOS root with a legacy daemon = %v, want it named", err)
	}
}

func TestStartManagerSilencesUsageBeforeLoadingConfig(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	c := &cobra.Command{Use: "run"}
	c.Flags().String("config", "", "")
	if err := c.Flags().Set("config", "/nonexistent/runner/config.toml"); err != nil {
		t.Fatal(err)
	}
	if err := startManager(c); err == nil {
		t.Fatal("startManager() with a missing config succeeded")
	}
	if !c.SilenceUsage {
		t.Fatal("a runtime failure would still print the full usage")
	}
}
```

`cmd/runner/logpath_test.go` 追加：

```go
func TestLogConsole(t *testing.T) {
	tests := []struct {
		discarded, fileOpen bool
		want                *os.File
	}{
		{false, false, os.Stdout},
		{false, true, os.Stdout},
		{true, true, os.Stdout},
		{true, false, os.Stderr},
	}
	for _, tt := range tests {
		if got := logConsole(tt.discarded, tt.fileOpen); got != tt.want {
			t.Errorf("logConsole(%v, %v) = %s, want %s", tt.discarded, tt.fileOpen, got.Name(), tt.want.Name())
		}
	}
}

func TestStdoutIsDevNull(t *testing.T) {
	saved := os.Stdout
	t.Cleanup(func() { os.Stdout = saved })

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	os.Stdout = null
	if !stdoutIsDevNull() {
		t.Error("stdout on /dev/null not detected")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	os.Stdout = w
	if stdoutIsDevNull() {
		t.Error("a pipe was reported as /dev/null")
	}
}
```

`cmd/runner/tool_path_test.go` 追加（import 補 `"os"`）：

```go
func TestHomebrewPathOnlyChangesMacOS(t *testing.T) {
	dir := t.TempDir()
	saved := homebrewBinDirs
	t.Cleanup(func() { homebrewBinDirs = saved })
	homebrewBinDirs = []string{dir}

	if got := homebrewPath("linux", "/usr/bin"); got != "/usr/bin" {
		t.Errorf("linux PATH = %q, want it unchanged", got)
	}
	if got := homebrewPath("darwin", "/usr/bin"); got != "/usr/bin:"+dir {
		t.Errorf("darwin PATH = %q, want %q", got, "/usr/bin:"+dir)
	}
}

func TestAppendMissingDirsSkipsFilesAndDuplicateCandidates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := appendMissingDirs("/usr/bin", []string{file, dir, dir}); got != "/usr/bin:"+dir {
		t.Errorf("appendMissingDirs() = %q, want %q", got, "/usr/bin:"+dir)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestRefuseDarwinRoot|TestStartManagerSilencesUsage|TestLogConsole|TestStdoutIsDevNull|TestHomebrewPath|TestAppendMissingDirs'`
Expected: FAIL，`undefined: refuseDarwinRoot`、`undefined: homebrewPath` 等編譯錯誤

- [ ] **Step 3: 實作**

`cmd/runner/platform_guard.go`：

```go
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// darwinSystemPlists are LaunchDaemon definitions older releases installed.
var darwinSystemPlists = []string{
	launchdSystemDir + "/" + launchdPlistFile,
	launchdSystemDir + "/" + legacyLaunchdPlist,
}

// refuseDarwinRoot stops root from running, installing or migrating runner on
// macOS: Tart keeps images in the invoking user's ~/.tart and Docker Desktop
// runs per user, so a root instance sees neither.
func refuseDarwinRoot(goos string, euid int, action string, exists func(string) bool) error {
	if goos != "darwin" || euid != 0 {
		return nil
	}
	msg := fmt.Sprintf("refusing to %s as root on macOS: run it as the logged-in user and install a LaunchAgent with 'runner service install'", action)
	var found []string
	for _, plist := range darwinSystemPlists {
		if exists(plist) {
			found = append(found, plist)
		}
	}
	if len(found) > 0 {
		msg += fmt.Sprintf("\n\n  Found system service definitions: %s\n  Convert them as described in the README section \"macOS LaunchDaemon to LaunchAgent\"", strings.Join(found, ", "))
	}
	return errors.New(msg)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
```

`cmd/runner/logpath.go` 追加：

```go
// stdoutIsDevNull reports whether stdout is discarded, as generated launchd
// agents set it.
func stdoutIsDevNull() bool {
	out, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(out, null)
}

// logConsole keeps log lines visible when stdout is discarded and no log
// file is open, by sending them to stderr instead.
func logConsole(stdoutDiscarded, fileOpen bool) *os.File {
	if stdoutDiscarded && !fileOpen {
		return os.Stderr
	}
	return os.Stdout
}
```

`cmd/runner/main.go`：
- `startManager` 開頭加入：

```go
	// Runtime failures are not usage errors; the full flag list buried the
	// actual cause in service logs.
	cmd.SilenceUsage = true
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "run runner", pathExists); err != nil {
		return err
	}
```

- `runManager` 的 logger 建立改為：

```go
	console := logConsole(stdoutIsDevNull(), logFile != nil)
	logger := config.NewLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, console, logWriter(logFile))
```

- scale set logger 改為 `config.NewScaleSetLoggerWithWriter(cfg.LogLevel, cfg.LogFormat, ss.ScaleSetName, i, console, logWriter(logFile))`
- import 補 `"runtime"`

`cmd/runner/tool_path.go` 的 `ensureHomebrewPath` 改為：

```go
// ensureHomebrewPath lets runner find tart when launchd starts it with
// PATH=/usr/bin:/bin:/usr/sbin:/sbin, without rewriting the plist or launchd's
// own configuration. Interactive shells already carry these directories, so
// they see no change.
func ensureHomebrewPath() {
	path := os.Getenv("PATH")
	if extended := homebrewPath(runtime.GOOS, path); extended != path {
		_ = os.Setenv("PATH", extended)
	}
}

// homebrewPath returns path with Homebrew's bin directories appended on macOS.
func homebrewPath(goos, path string) string {
	if goos != "darwin" {
		return path
	}
	return appendMissingDirs(path, homebrewBinDirs)
}
```

- [ ] **Step 4: 確認測試通過**

Run: `go test ./cmd/runner -count=1`
Expected: PASS

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add cmd/runner/platform_guard.go cmd/runner/platform_guard_test.go cmd/runner/logpath.go cmd/runner/logpath_test.go cmd/runner/main.go cmd/runner/tool_path.go cmd/runner/tool_path_test.go
git commit -m "feat(run): refuse root on macOS and keep logs when stdout is discarded"
```

---

### Task 8: service 範本（systemd 沙箱與跳脫、launchd stderr、`ExecStart` 解析）

**Files:**
- Modify: `cmd/runner/cmd_service.go`（`installOpts`、systemd／launchd 範本與 render、`launchdManager.install`、`launchdManager.logs`）
- Modify: `cmd/runner/cmd_migrate.go`（`parseSystemdServiceInvocation` 改用 `splitSystemdCommand`）
- Modify: `cmd/runner/cmd_service_test.go`

**Interfaces:**
- Consumes: `serviceVersionEnv`、`serviceStopTimeoutEnv`、`currentServiceVersion`（Task 6）；`layout.SystemLogsDirectory`、`layout.SystemLogRoot`、`layout.SystemLogFile`（Task 1）；`runnerlock.DefaultPath`
- Produces:
  - `installOpts` 新增欄位 `logFile *string`、`xdgConfigHome string`、`xdgStateHome string`
  - `type envVar struct { Key, Value string }`、`func serviceEnvironment(opts installOpts) []envVar`
  - `func systemdLogsDirectory(logFile *string) (dir, warning string)`
  - `func systemdExecArg(s string) (string, error)`、`func systemdValue(s string) (string, error)`
  - `func splitSystemdCommand(s string) ([]string, error)`
  - `func xmlEscape(s string) string`
  - `func launchdStderrPath(user bool) string`、`func launchdLegacyLogPath(user bool) string`
  - 測試 helper：`func unitValues(unit, key string) []string`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/cmd_service_test.go`：刪除 `TestSystemdUnitSystemModeKeepsLockDirWritable` 與 `readWritePaths`；新增下列測試（import 補 `"encoding/xml"`、`"io"`、`"os/exec"`、`"runtime"`）：

```go
func unitValues(unit, key string) []string {
	var values []string
	for line := range strings.SplitSeq(unit, "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			values = append(values, strings.Fields(value)...)
		}
	}
	return values
}

func TestSystemdUnitSystemModeSandbox(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{
		binaryPath: "/usr/local/bin/runner",
		configPath: "/etc/runner/config.toml",
		provider:   "docker",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := unitValues(unit, "ReadWritePaths"); !slices.Equal(got, []string{"/tmp"}) {
		t.Errorf("ReadWritePaths = %q, want only /tmp (lock); config and socket stay read-only", got)
	}
	if got := unitValues(unit, "LogsDirectory"); !slices.Equal(got, []string{"runner"}) {
		t.Errorf("LogsDirectory = %q, want runner", got)
	}
	for _, want := range []string{
		"ProtectSystem=strict",
		"NoNewPrivileges=true",
		`Environment="RUNNER_SERVICE_VERSION=2"`,
		`Environment="RUNNER_SERVICE_STOP_TIMEOUT=7260"`,
		`ExecStart="/usr/local/bin/runner" run --config "/etc/runner/config.toml"`,
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("system unit missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdLogsDirectory(t *testing.T) {
	str := func(s string) *string { return &s }
	tests := []struct {
		name        string
		logFile     *string
		wantDir     string
		wantWarning bool
	}{
		{name: "unset", wantDir: "runner"},
		{name: "subdirectory", logFile: str("/var/log/custom/runner.log"), wantDir: "custom"},
		{name: "nested subdirectory", logFile: str("/var/log/ci/runner/runner.log"), wantDir: "ci/runner"},
		{name: "directly in var log", logFile: str("/var/log/runner.log"), wantDir: "runner", wantWarning: true},
		{name: "outside var log", logFile: str("/home/ada/runner/runner.log"), wantDir: "runner", wantWarning: true},
		{name: "relative", logFile: str("logs/runner.log"), wantDir: "runner", wantWarning: true},
		{name: "unsafe characters", logFile: str("/var/log/has space/runner.log"), wantDir: "runner", wantWarning: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, warning := systemdLogsDirectory(tt.logFile)
			if dir != tt.wantDir || (warning != "") != tt.wantWarning {
				t.Errorf("systemdLogsDirectory() = %q, %q; want %q, warning %v", dir, warning, tt.wantDir, tt.wantWarning)
			}
		})
	}
}

func TestSystemdUnitUsesCustomLogsDirectory(t *testing.T) {
	logFile := "/var/log/custom/runner.log"
	unit, err := renderSystemdUnit(installOpts{binaryPath: "/usr/local/bin/runner", configPath: "/etc/runner/config.toml", logFile: &logFile})
	if err != nil {
		t.Fatal(err)
	}
	if got := unitValues(unit, "LogsDirectory"); !slices.Equal(got, []string{"custom"}) {
		t.Errorf("LogsDirectory = %q, want custom", got)
	}
}

func TestSystemdUnitEscapesExecStartArguments(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{binaryPath: `/opt/my runner/100%/$HOME/run"er`, configPath: "/etc/runner/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/opt/my runner/100%%/$$HOME/run\"er" run --config "/etc/runner/config.toml"`
	if !strings.Contains(unit, want) {
		t.Errorf("unit missing %q:\n%s", want, unit)
	}
}

func TestSystemdUnitRejectsControlCharacters(t *testing.T) {
	if _, err := renderSystemdUnit(installOpts{binaryPath: "/opt/run\nner", configPath: "/etc/runner/config.toml"}); err == nil {
		t.Fatal("a newline in the binary path was written into the unit")
	}
	if _, err := renderSystemdUnit(installOpts{user: true, binaryPath: "/opt/runner", configPath: "/etc/runner/config.toml", xdgStateHome: "/xdg/\tstate"}); err == nil {
		t.Fatal("a tab in an environment value was written into the unit")
	}
}

func TestSystemdExecStartRoundTrip(t *testing.T) {
	for _, tc := range []struct{ binary, config string }{
		{"/usr/local/bin/runner", "/etc/runner/config.toml"},
		{`/opt/my runner/run"er`, `/data/100%/$HOME/c\fg.toml`},
	} {
		unit, err := renderSystemdUnit(installOpts{binaryPath: tc.binary, configPath: tc.config})
		if err != nil {
			t.Fatal(err)
		}
		invocation, err := parseSystemdServiceInvocation([]byte(unit))
		if err != nil {
			t.Fatal(err)
		}
		if invocation.BinaryPath != tc.binary || invocation.ConfigPath != tc.config {
			t.Errorf("round trip = %+v, want binary %q config %q", invocation, tc.binary, tc.config)
		}
	}
}

func TestSplitSystemdCommandRejectsUnterminatedQuote(t *testing.T) {
	if _, err := splitSystemdCommand(`"/usr/local/bin/runner run`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestSystemdUserUnitRecordsAbsoluteXDG(t *testing.T) {
	unit, err := renderSystemdUnit(installOpts{
		user: true, binaryPath: "/home/ada/.local/bin/runner", configPath: "/home/ada/.config/runner/config.toml",
		xdgStateHome: "/xdg/state", xdgConfigHome: "relative/config",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unit, `Environment="XDG_STATE_HOME=/xdg/state"`) || strings.Contains(unit, "XDG_CONFIG_HOME") {
		t.Errorf("user unit XDG environment wrong:\n%s", unit)
	}
	if strings.Contains(unit, "LogsDirectory=") || strings.Contains(unit, "ReadWritePaths=") {
		t.Errorf("user unit must not carry the system sandbox:\n%s", unit)
	}
}

func TestLaunchdPlistDiscardsStdoutAndRecordsEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	plist, err := renderLaunchdPlist(installOpts{
		user: true, binaryPath: "/Users/ada/.local/bin/runner", configPath: "/Users/ada/.config/runner/config.toml",
		xdgStateHome: "/xdg/state",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>StandardOutPath</key>\n    <string>/dev/null</string>",
		"<key>StandardErrorPath</key>\n    <string>" + filepath.Join(home, "Library", "Logs", "runner", "stderr.log") + "</string>",
		"<key>RUNNER_SERVICE_VERSION</key>\n        <string>2</string>",
		"<key>RUNNER_SERVICE_STOP_TIMEOUT</key>\n        <string>7260</string>",
		"<key>XDG_STATE_HOME</key>\n        <string>/xdg/state</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
}

func TestLaunchdPlistEscapesXML(t *testing.T) {
	binary := "/Users/a&b/<bin>/runner"
	plist, err := renderLaunchdPlist(installOpts{user: true, binaryPath: binary, configPath: "/Users/a&b/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := parseLaunchdServiceInvocation([]byte(plist))
	if err != nil {
		t.Fatalf("plist is not parseable: %v\n%s", err, plist)
	}
	if invocation.BinaryPath != binary || invocation.ConfigPath != "/Users/a&b/config.toml" {
		t.Errorf("XML round trip = %+v", invocation)
	}
	decoder := xml.NewDecoder(strings.NewReader(plist))
	decoder.Strict = false // the DOCTYPE is not resolved
	for {
		if _, err := decoder.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not well-formed: %v", err)
		}
	}
}

func TestLaunchdPlistPassesPlutil(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS-only")
	}
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not found")
	}
	plist, err := renderLaunchdPlist(installOpts{user: true, binaryPath: "/Users/a&b/runner", configPath: "/Users/a b/config.toml"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runner.plist")
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, out)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestSystemd|TestLaunchd|TestSplitSystemdCommand'`
Expected: FAIL，`unknown field logFile in struct literal`、`undefined: systemdLogsDirectory`、`undefined: splitSystemdCommand` 等編譯錯誤

- [ ] **Step 3: 實作 systemd 範本與跳脫**

`cmd/runner/cmd_service.go` 的 `installOpts` 追加欄位：

```go
	// logFile is the config's explicit log-file (nil when unset); a system
	// unit can only grant it a directory under /var/log.
	logFile *string
	// XDG base directories of the installing shell, recorded in user
	// services so the service and the CLI resolve the same paths.
	xdgConfigHome string
	xdgStateHome  string
```

systemd 範本、資料與 render 改為：

```go
var systemdTmpl = template.Must(template.New("systemd").Parse(`[Unit]
Description={{.Description}}
{{- /* User units cannot depend on system units like docker.service —
       systemd would fail with "Unit docker.service not found". */}}
{{- if and .AfterDocker (not .User)}}
After=docker.service
Requires=docker.service
{{- end}}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.ExecStart}}
Restart=on-failure
RestartSec=10s
# Must exceed runner's drain budget so systemd does not SIGKILL an in-flight job.
TimeoutStopSec={{.StopTimeoutSeconds}}
{{- range .Environment}}
Environment={{.}}
{{- end}}
{{- if not .User}}
NoNewPrivileges=true
ProtectSystem=strict
LogsDirectory={{.LogsDirectory}}
ReadWritePaths={{.ReadWritePaths}}
{{- end}}

[Install]
WantedBy={{- if .User}}default.target{{- else}}multi-user.target{{- end}}
`))

type systemdData struct {
	Description        string
	ExecStart          string
	AfterDocker        bool
	User               bool
	LogsDirectory      string
	ReadWritePaths     string
	StopTimeoutSeconds int
	Environment        []string
}

// renderSystemdUnit renders the complete unit used by install. Keeping
// rendering separate from filesystem writes makes the sandbox directly testable.
func renderSystemdUnit(opts installOpts) (string, error) {
	binary, err := systemdExecArg(opts.binaryPath)
	if err != nil {
		return "", fmt.Errorf("binary path: %w", err)
	}
	configPath, err := systemdExecArg(opts.configPath)
	if err != nil {
		return "", fmt.Errorf("config path: %w", err)
	}
	logsDir, _ := systemdLogsDirectory(opts.logFile)
	data := systemdData{
		Description:   serviceDescription,
		ExecStart:     binary + " run --config " + configPath,
		AfterDocker:   opts.provider == "docker",
		User:          opts.user,
		LogsDirectory: logsDir,
		// ProtectSystem=strict leaves /tmp read-only, where runner run creates
		// its machine-wide lock. PrivateTmp would not help: a private /tmp
		// hides the lock from runners started outside the service. Unix
		// sockets such as Docker's stay connectable on read-only paths.
		ReadWritePaths:     filepath.Dir(runnerlock.DefaultPath),
		StopTimeoutSeconds: int(serviceStopTimeout(opts.drainTimeout).Seconds()),
	}
	for _, v := range serviceEnvironment(opts) {
		assignment, err := systemdValue(v.Key + "=" + v.Value)
		if err != nil {
			return "", fmt.Errorf("environment %s: %w", v.Key, err)
		}
		data.Environment = append(data.Environment, assignment)
	}
	var out strings.Builder
	if err := systemdTmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render systemd unit: %w", err)
	}
	return out.String(), nil
}

type envVar struct{ Key, Value string }

// serviceEnvironment is what every generated definition records: the template
// generation and stop timeout runner checks at startup, and for user services
// the installing shell's absolute XDG directories.
func serviceEnvironment(opts installOpts) []envVar {
	env := []envVar{
		{Key: serviceVersionEnv, Value: strconv.Itoa(currentServiceVersion)},
		{Key: serviceStopTimeoutEnv, Value: strconv.Itoa(int(serviceStopTimeout(opts.drainTimeout).Seconds()))},
	}
	if opts.user {
		if filepath.IsAbs(opts.xdgConfigHome) {
			env = append(env, envVar{Key: "XDG_CONFIG_HOME", Value: opts.xdgConfigHome})
		}
		if filepath.IsAbs(opts.xdgStateHome) {
			env = append(env, envVar{Key: "XDG_STATE_HOME", Value: opts.xdgStateHome})
		}
	}
	return env
}

// systemdLogsDirectory maps an explicit log-file onto LogsDirectory=, the only
// log location a system unit's sandbox can write. It applies the same rule as
// root's runtime log decision, so a rejected path is rejected in both places.
func systemdLogsDirectory(logFile *string) (dir, warning string) {
	const fallback = "runner"
	if logFile == nil || *logFile == "" {
		return fallback, ""
	}
	if rel, ok := layout.SystemLogsDirectory(*logFile); ok {
		return rel, ""
	}
	return fallback, fmt.Sprintf("log-file %s is not in a directory under %s named with letters, digits, '.', '_' or '-'; the service will write to %s instead", *logFile, layout.SystemLogRoot, layout.SystemLogFile)
}

// systemdExecArg quotes one ExecStart word. systemd removes the quotes and
// C-style escapes and expands %specifiers and $VARIABLES, so a literal % or $
// is doubled.
func systemdExecArg(s string) (string, error) {
	if err := rejectControlChars(s); err != nil {
		return "", err
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(s) + `"`, nil
}

// systemdValue quotes a directive value such as an Environment= assignment,
// where specifiers expand but $VARIABLES do not.
func systemdValue(s string) (string, error) {
	if err := rejectControlChars(s); err != nil {
		return "", err
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`, nil
}

// rejectControlChars refuses values that would break line-based unit files.
func rejectControlChars(s string) error {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return fmt.Errorf("%q contains a control character", s)
	}
	return nil
}
```

`cmd/runner/cmd_migrate.go` 新增並改用：

```go
// splitSystemdCommand splits an ExecStart value into words, undoing the
// quoting systemdExecArg applies and accepting the unquoted form older
// releases wrote.
func splitSystemdCommand(s string) ([]string, error) {
	var (
		words   []string
		current strings.Builder
		quote   rune
		inWord  bool
	)
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0 && r == '\\':
			if i+1 >= len(runes) {
				return nil, errors.New("ExecStart ends with a dangling backslash")
			}
			i++
			current.WriteRune(runes[i])
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, inWord = r, true
		case quote == 0 && unicode.IsSpace(r):
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("ExecStart has an unterminated quote")
	}
	if inWord {
		words = append(words, current.String())
	}
	unescape := strings.NewReplacer("%%", "%", "$$", "$")
	for i, word := range words {
		words[i] = unescape.Replace(word)
	}
	return words, nil
}
```

`parseSystemdServiceInvocation` 內：

```go
		args := strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		for i := range args {
			args[i] = strings.Trim(args[i], "\"'")
		}
		return invocationFromArgs(args)
```

改為：

```go
		args, err := splitSystemdCommand(strings.TrimPrefix(line, "ExecStart="))
		if err != nil {
			return legacyServiceInvocation{}, err
		}
		return invocationFromArgs(args)
```

`cmd_migrate.go` import 補 `"unicode"`。

- [ ] **Step 4: 實作 launchd 範本**

`cmd/runner/cmd_service.go`：

```go
var launchdTmpl = template.Must(template.New("launchd").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{xml .Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{xml .BinaryPath}}</string>
        <string>run</string>
        <string>--config</string>
        <string>{{xml .ConfigPath}}</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
{{- range .Environment}}
        <key>{{xml .Key}}</key>
        <string>{{xml .Value}}</string>
{{- end}}
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <!-- Must exceed runner's drain budget so launchd does not SIGKILL an in-flight job. -->
    <key>ExitTimeOut</key>
    <integer>{{.StopTimeoutSeconds}}</integer>
    <!-- runner writes its own rotated log; stdout would only duplicate it. -->
    <key>StandardOutPath</key>
    <string>/dev/null</string>
    <key>StandardErrorPath</key>
    <string>{{xml .StderrPath}}</string>
</dict>
</plist>
`))

type launchdData struct {
	Label              string
	BinaryPath         string
	ConfigPath         string
	StderrPath         string
	StopTimeoutSeconds int
	Environment        []envVar
}

// renderLaunchdPlist renders the complete plist used by install.
func renderLaunchdPlist(opts installOpts) (string, error) {
	data := launchdData{
		Label:              launchdLabel,
		BinaryPath:         opts.binaryPath,
		ConfigPath:         opts.configPath,
		StderrPath:         launchdStderrPath(opts.user),
		StopTimeoutSeconds: int(serviceStopTimeout(opts.drainTimeout).Seconds()),
		Environment:        serviceEnvironment(opts),
	}
	var out strings.Builder
	if err := launchdTmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("render launchd plist: %w", err)
	}
	return out.String(), nil
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// launchdStderrPath keeps what runner cannot log itself: startup failures,
// panics and child process stderr. It is not rotated.
func launchdStderrPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "runner", "stderr.log")
	}
	return "/var/log/runner/stderr.log"
}

// launchdLegacyLogPath is the combined stdout and stderr file of plists
// generated before stdout was discarded.
func launchdLegacyLogPath(user bool) string {
	if user {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Logs", "runner.log")
	}
	return "/var/log/runner.log"
}
```

刪除 `launchdManager.logPath`。`launchdManager.install` 在 `// Check if already installed` 之前加入：

```go
	if err := os.MkdirAll(filepath.Dir(launchdStderrPath(opts.user)), 0o700); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}
```

`launchdManager.logs` 的 `logFile := m.logPath(user)` 暫改為 `logFile := launchdStderrPath(user)`（Task 9 再加 fallback）。import 補 `"encoding/xml"`、`"strconv"`、`"unicode"`。

- [ ] **Step 5: 確認測試通過**

Run: `go test ./cmd/runner -count=1 -run 'TestSystemd|TestLaunchd|TestSplitSystemdCommand|TestServiceTemplates|TestParseLegacyServiceInvocation|TestMigrate' -v`
Expected: 全部 PASS（`TestLaunchdPlistPassesPlutil` 在 Linux 上 SKIP）

- [ ] **Step 6: 全綠檢查並提交**

```bash
git add cmd/runner/cmd_service.go cmd/runner/cmd_service_test.go cmd/runner/cmd_migrate.go
git commit -m "feat(service): generate sandboxed, escaped units and bounded launchd logging"
```

---

### Task 9: service 安裝流程（config 驗證、binary 驗證、`--force`、uninstall 舊版、logs、status）

**Files:**
- Modify: `cmd/runner/cmd_service.go`
- Modify: `cmd/runner/legacy.go`（新增 `removeLegacyServiceFile`）
- Modify: `cmd/runner/cmd_migrate.go`（`defaultMigrationServiceDeps.removeLegacyFile` 改用 `removeLegacyServiceFile`）
- Modify: `cmd/runner/cmd_service_test.go`

**Interfaces:**
- Consumes: `checkRootOnlyChain`、`lstatFile`、`statFunc`、`fakeStat`、`rootDir`（Task 2）；`envFrom`（Task 6 測試）；`refuseDarwinRoot`、`pathExists`（Task 7）；`systemdLogsDirectory`、`renderSystemdUnit`、`renderLaunchdPlist`、`launchdStderrPath`、`launchdLegacyLogPath`（Task 8）；`writeFileAtomic`、`shellQuotePath`（`migrate_config.go`）；`fakeMigrationServiceManager`（`cmd_migrate_test.go`）
- Produces:
  - `installOpts` 新增 `force bool`
  - `var errServiceNotInstalled = errors.New("service not installed")`
  - `type serviceConfigFacts struct { found bool; provider string; drainTimeout *time.Duration; logFile *string }`
  - `func readServiceConfig(configPath string, required bool) (serviceConfigFacts, error)`
  - `func buildInstallOpts(user bool, configPath, binaryPath string, requireConfig bool, getenv func(string) string) (installOpts, []string, error)`
  - `func resolveBinaryPath(cmd *cobra.Command) (string, error)`（行為改為一律解析 symlink）
  - `func validateServiceBinary(goos string, user bool, path string, evalSymlinks func(string) (string, error), stat statFunc) (string, error)`
  - `func validateServiceBinaryFor(user bool, path string) (string, error)`
  - `var renderServiceDefinition func(opts installOpts) (string, error)`
  - `func installService(mgr serviceManager, opts installOpts, validateBinary func(user bool, path string) (string, error)) error`
  - `func uninstallServices(user bool, uninstallCurrent func(bool) error, legacyInstalled func(bool) bool, removeLegacy func(bool) error) (removedLegacy bool, err error)`
  - `func launchdServiceLogFile(user bool, exists func(string) bool) (path, note string)`
  - `func statusPrivilegeError(goos string, user bool, euid int) error`
  - `func removeLegacyServiceFile(user bool) error`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/cmd_service_test.go` 追加（import 補 `"errors"`、`"io/fs"`、`"github.com/spf13/cobra"`）：

```go
func TestReadServiceConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := write("valid.toml", "provider = \"tart\"\ndrain-timeout = \"3h\"\nlog-file = \"/var/log/custom/runner.log\"\n")
	broken := write("broken.toml", "provider = \n")
	missing := filepath.Join(dir, "missing.toml")

	facts, err := readServiceConfig(valid, true)
	if err != nil || !facts.found || facts.provider != "tart" ||
		facts.drainTimeout == nil || *facts.drainTimeout != 3*time.Hour ||
		facts.logFile == nil || *facts.logFile != "/var/log/custom/runner.log" {
		t.Fatalf("valid config facts = %+v, %v", facts, err)
	}
	if _, err := readServiceConfig(broken, false); err == nil {
		t.Fatal("a config with a syntax error was accepted")
	}
	if facts, err := readServiceConfig(missing, false); err != nil || facts.found {
		t.Fatalf("missing optional config = %+v, %v", facts, err)
	}
	if _, err := readServiceConfig(missing, true); err == nil {
		t.Fatal("a missing required config was accepted")
	}
}

func TestBuildInstallOptsRecordsConfigFactsAndXDG(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(valid, []byte("provider = \"tart\"\nlog-file = \"/var/log/custom/runner.log\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envFrom(map[string]string{"XDG_STATE_HOME": "/xdg/state", "XDG_CONFIG_HOME": "/xdg/config"})

	opts, warnings, err := buildInstallOpts(true, valid, "/usr/local/bin/runner", true, env)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("buildInstallOpts() warnings=%q err=%v", warnings, err)
	}
	if !opts.user || opts.configPath != valid || opts.binaryPath != "/usr/local/bin/runner" || opts.provider != "tart" ||
		opts.logFile == nil || *opts.logFile != "/var/log/custom/runner.log" ||
		opts.xdgStateHome != "/xdg/state" || opts.xdgConfigHome != "/xdg/config" {
		t.Fatalf("installOpts = %+v", opts)
	}

	missing := filepath.Join(dir, "missing.toml")
	if _, warnings, err := buildInstallOpts(true, missing, "/usr/local/bin/runner", false, env); err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "Config file not found") {
		t.Fatalf("fresh install without config: warnings=%q err=%v", warnings, err)
	}
	if _, _, err := buildInstallOpts(true, missing, "/usr/local/bin/runner", true, env); err == nil {
		t.Fatal("--force without a config was accepted")
	}
}

func TestValidateServiceBinary(t *testing.T) {
	safe := map[string]fileStat{
		"/": rootDir(0o755), "/usr": rootDir(0o755), "/usr/local": rootDir(0o755),
		"/usr/local/bin": rootDir(0o755), "/usr/local/bin/runner": {UID: 0, Mode: 0o755},
	}
	unsafe := map[string]fileStat{
		"/": rootDir(0o755), "/home": rootDir(0o755), "/home/ada": {UID: 1000, Mode: fs.ModeDir | 0o755},
		"/home/ada/runner": {UID: 1000, Mode: 0o755},
	}
	resolve := func(path string) (string, error) {
		switch path {
		case "/usr/local/bin/link":
			return "/usr/local/bin/runner", nil
		case "/missing":
			return "", fs.ErrNotExist
		}
		return path, nil
	}
	tests := []struct {
		name    string
		goos    string
		user    bool
		path    string
		stat    map[string]fileStat
		want    string
		wantErr string
	}{
		{name: "linux system resolves the symlink then passes", goos: "linux", path: "/usr/local/bin/link", stat: safe, want: "/usr/local/bin/runner"},
		{name: "linux system user-owned binary", goos: "linux", path: "/home/ada/runner", stat: unsafe, wantErr: "sudo install -m 0755"},
		{name: "linux user service skips the check", goos: "linux", user: true, path: "/home/ada/runner", stat: unsafe, want: "/home/ada/runner"},
		{name: "unresolvable binary", goos: "linux", path: "/missing", stat: safe, wantErr: "resolve binary"},
		{name: "system binary that is a directory", goos: "linux", path: "/usr/local/bin", stat: safe, wantErr: "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateServiceBinary(tt.goos, tt.user, tt.path, resolve, fakeStat(tt.stat))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateServiceBinary() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("validateServiceBinary() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestInstallServiceValidatesBeforeTouchingExistingDefinition(t *testing.T) {
	var actions []string
	installed, running := true, true
	manager := &fakeMigrationServiceManager{actions: &actions, installed: &installed, running: &running}
	opts := installOpts{binaryPath: "/home/ada/runner", configPath: "/etc/runner/config.toml", force: true}

	err := installService(manager, opts, func(bool, string) (string, error) { return "", errors.New("unsafe binary") })
	if err == nil || len(actions) != 0 {
		t.Fatalf("failed validation: err=%v actions=%v, want no manager action", err, actions)
	}

	saved := renderServiceDefinition
	t.Cleanup(func() { renderServiceDefinition = saved })
	renderServiceDefinition = func(installOpts) (string, error) { return "", errors.New("render failed") }
	err = installService(manager, opts, func(_ bool, p string) (string, error) { return p, nil })
	if err == nil || len(actions) != 0 {
		t.Fatalf("failed render: err=%v actions=%v, want no manager action", err, actions)
	}

	renderServiceDefinition = saved
	err = installService(manager, opts, func(bool, string) (string, error) { return "/usr/local/bin/runner", nil })
	if err != nil || strings.Join(actions, ",") != "install-new" || manager.installOpts.binaryPath != "/usr/local/bin/runner" {
		t.Fatalf("valid install: err=%v actions=%v opts=%+v", err, actions, manager.installOpts)
	}
}

func TestUninstallServicesRemovesLegacyDefinitions(t *testing.T) {
	notInstalled := func(bool) error { return fmt.Errorf("%w (no unit file)", errServiceNotInstalled) }
	removed := func(bool) error { return nil }
	tests := []struct {
		name        string
		current     func(bool) error
		legacy      bool
		removeErr   error
		wantLegacy  bool
		wantErrIs   error
		wantErrText string
	}{
		{name: "only legacy present", current: notInstalled, legacy: true, wantLegacy: true},
		{name: "both present", current: removed, legacy: true, wantLegacy: true},
		{name: "only current present", current: removed},
		{name: "nothing installed", current: notInstalled, wantErrIs: errServiceNotInstalled},
		{name: "legacy removal fails", current: removed, legacy: true, removeErr: errors.New("busy"), wantErrText: "busy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLegacy, err := uninstallServices(false, tt.current, func(bool) bool { return tt.legacy }, func(bool) error { return tt.removeErr })
			if gotLegacy != tt.wantLegacy {
				t.Errorf("removedLegacy = %v, want %v", gotLegacy, tt.wantLegacy)
			}
			switch {
			case tt.wantErrIs != nil:
				if !errors.Is(err, tt.wantErrIs) {
					t.Errorf("err = %v, want %v", err, tt.wantErrIs)
				}
			case tt.wantErrText != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("err = %v, want %q", err, tt.wantErrText)
				}
			case err != nil:
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

func TestLaunchdServiceLogFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := filepath.Join(home, "Library", "Logs", "runner", "stderr.log")
	legacy := filepath.Join(home, "Library", "Logs", "runner.log")
	only := func(paths ...string) func(string) bool {
		return func(p string) bool { return slices.Contains(paths, p) }
	}
	if path, note := launchdServiceLogFile(true, only(current, legacy)); path != current || note != "" {
		t.Errorf("with the current log = %q, %q", path, note)
	}
	if path, note := launchdServiceLogFile(true, only(legacy)); path != legacy || !strings.Contains(note, "--force") {
		t.Errorf("with only the legacy log = %q, %q", path, note)
	}
	if path, _ := launchdServiceLogFile(true, only()); path != "" {
		t.Errorf("with no log = %q, want empty", path)
	}
}

func TestStatusPrivilegeError(t *testing.T) {
	if err := statusPrivilegeError("darwin", false, 501); err == nil || !strings.Contains(err.Error(), "sudo runner service status --user=false") {
		t.Errorf("macOS system status as a user = %v", err)
	}
	for _, tc := range []struct {
		goos string
		user bool
		euid int
	}{{"darwin", true, 501}, {"darwin", false, 0}, {"linux", false, 1000}} {
		if err := statusPrivilegeError(tc.goos, tc.user, tc.euid); err != nil {
			t.Errorf("statusPrivilegeError(%+v) = %v, want nil", tc, err)
		}
	}
}

func TestResolveBinaryPathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "runner")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "runner-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	c := &cobra.Command{}
	c.Flags().String("binary-path", "", "")
	if err := c.Flags().Set("binary-path", link); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBinaryPath(c)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got != want {
		t.Fatalf("resolveBinaryPath() = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestReadServiceConfig|TestBuildInstallOpts|TestValidateServiceBinary|TestInstallService|TestUninstallServices|TestLaunchdServiceLogFile|TestStatusPrivilegeError|TestResolveBinaryPath'`
Expected: FAIL，`undefined: readServiceConfig` 等編譯錯誤

- [ ] **Step 3: 實作**

`cmd/runner/legacy.go` 追加（import 補 `"runtime"`）：

```go
// removeLegacyServiceFile stops and removes a pre-rename service definition.
func removeLegacyServiceFile(user bool) error {
	switch runtime.GOOS {
	case "linux":
		return removeSystemdUnit(user, legacySystemdUnit, legacyServiceName)
	case "darwin":
		return removeLaunchdPlist(legacyLaunchdPlistPath(user))
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}
```

`cmd/runner/cmd_migrate.go` 的 `defaultMigrationServiceDeps` 中 `removeLegacyFile` 改為 `removeLegacyFile: removeLegacyServiceFile,`。

`cmd/runner/cmd_service.go`：`installOpts` 追加 `force bool`；install flags 追加：

```go
	f.Bool("force", false, "Regenerate an existing service definition after validating the new one")
```

新增與替換的函式：

```go
var errServiceNotInstalled = errors.New("service not installed")

// serviceConfigFacts are the parts of a config a service definition encodes.
type serviceConfigFacts struct {
	found        bool
	provider     string
	drainTimeout *time.Duration
	logFile      *string
}

// readServiceConfig loads the facts a service definition depends on. A config
// that exists must read and load; a missing one is allowed only when not
// required.
func readServiceConfig(configPath string, required bool) (serviceConfigFacts, error) {
	facts := serviceConfigFacts{provider: platformDefaultProvider()}
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		if required {
			return facts, fmt.Errorf("config %s does not exist", configPath)
		}
		return facts, nil
	} else if err != nil {
		return facts, fmt.Errorf("inspect config %s: %w", configPath, err)
	}
	v := viper.New()
	v.SetConfigFile(configPath)
	if err := v.ReadInConfig(); err != nil {
		return facts, fmt.Errorf("read config %s: %w", configPath, err)
	}
	cfg, err := config.Load(v)
	if err != nil {
		return facts, fmt.Errorf("load config %s: %w", configPath, err)
	}
	facts.found = true
	facts.drainTimeout = cfg.DrainTimeout
	facts.logFile = cfg.LogFile
	if sets := cfg.ResolveScaleSets(); len(sets) > 0 {
		facts.provider = "tart"
		for _, ss := range sets {
			if !ss.IsTart() {
				facts.provider = config.DefaultProvider
				break
			}
		}
	}
	return facts, nil
}

// buildInstallOpts gathers everything a service definition depends on from
// one read of the config. A missing config is only a warning for a fresh
// install; --force and migration require one.
func buildInstallOpts(user bool, configPath, binaryPath string, requireConfig bool, getenv func(string) string) (installOpts, []string, error) {
	facts, err := readServiceConfig(configPath, requireConfig)
	if err != nil {
		return installOpts{}, nil, err
	}
	var warnings []string
	if !facts.found {
		warnings = append(warnings, fmt.Sprintf("Config file not found at %s; run 'runner init' to generate one first", configPath))
	}
	if !user && runtime.GOOS == "linux" {
		if _, warning := systemdLogsDirectory(facts.logFile); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return installOpts{
		user:          user,
		configPath:    configPath,
		binaryPath:    binaryPath,
		provider:      facts.provider,
		drainTimeout:  facts.drainTimeout,
		logFile:       facts.logFile,
		xdgConfigHome: getenv("XDG_CONFIG_HOME"),
		xdgStateHome:  getenv("XDG_STATE_HOME"),
	}, warnings, nil
}

func runServiceInstall(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	noStart, _ := cmd.Flags().GetBool("no-start")
	force, _ := cmd.Flags().GetBool("force")

	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "install the runner service", pathExists); err != nil {
		return err
	}
	if err := checkPrivileges(user); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	binaryPath, err := resolveBinaryPath(cmd)
	if err != nil {
		return err
	}
	configPath, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	opts, warnings, err := buildInstallOpts(user, configPath, binaryPath, force, os.Getenv)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "  ⚠ %s\n", w)
	}
	opts.noStart = noStart
	opts.force = force
	return installService(mgr, opts, validateServiceBinaryFor)
}

// renderServiceDefinition renders what install would write, so every render
// failure surfaces before an existing service is touched.
var renderServiceDefinition = func(opts installOpts) (string, error) {
	if runtime.GOOS == "darwin" {
		return renderLaunchdPlist(opts)
	}
	return renderSystemdUnit(opts)
}

// installService validates the binary and renders the definition before the
// manager writes anything, so a failed --force leaves the installed service
// as it was. Every entry point that installs a service goes through here.
func installService(mgr serviceManager, opts installOpts, validateBinary func(user bool, path string) (string, error)) error {
	binaryPath, err := validateBinary(opts.user, opts.binaryPath)
	if err != nil {
		return err
	}
	opts.binaryPath = binaryPath
	if _, err := renderServiceDefinition(opts); err != nil {
		return err
	}
	return mgr.install(opts)
}

// validateServiceBinary resolves the binary a service will run and, for a
// system service on Linux, requires that only root can change it: a root
// service executing a user-writable file hands that user root.
func validateServiceBinary(goos string, user bool, path string, evalSymlinks func(string) (string, error), stat statFunc) (string, error) {
	resolved, err := evalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve binary %s: %w", path, err)
	}
	if user || goos != "linux" {
		return resolved, nil
	}
	st, err := stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect binary %s: %w", resolved, err)
	}
	if !st.Mode.IsRegular() {
		return "", fmt.Errorf("binary %s is not a regular file", resolved)
	}
	if err := checkRootOnlyChain(resolved, stat); err != nil {
		return "", fmt.Errorf("a system service must run a binary only root can modify: %w\n\n  Install it to a root-owned directory first:\n    sudo install -m 0755 %s /usr/local/bin/runner\n    sudo /usr/local/bin/runner service install --user=false --binary-path /usr/local/bin/runner", err, shellQuotePath(resolved))
	}
	return resolved, nil
}

func validateServiceBinaryFor(user bool, path string) (string, error) {
	return validateServiceBinary(runtime.GOOS, user, path, filepath.EvalSymlinks, lstatFile)
}

// resolveBinaryPath returns the real file the service will execute; the
// security check must see the same file the definition runs.
func resolveBinaryPath(cmd *cobra.Command) (string, error) {
	path, _ := cmd.Flags().GetString("binary-path")
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("cannot detect binary path: %w (use --binary-path)", err)
		}
		path = exe
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve binary %s: %w", abs, err)
	}
	return resolved, nil
}

func runServiceUninstall(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := checkPrivileges(user); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	removedLegacy, err := uninstallServices(user, mgr.uninstall, legacyServiceInstalled, removeLegacyServiceFile)
	if removedLegacy {
		fmt.Printf("  ✓ Legacy runscaler service removed\n")
	}
	return err
}

// uninstallServices removes runner's service and any pre-rename definition at
// the same level; finding only the legacy one is not an error.
func uninstallServices(user bool, uninstallCurrent func(bool) error, legacyInstalled func(bool) bool, removeLegacy func(bool) error) (removedLegacy bool, err error) {
	currentErr := uninstallCurrent(user)
	if legacyInstalled(user) {
		if err := removeLegacy(user); err != nil {
			return false, errors.Join(currentErr, fmt.Errorf("remove legacy service: %w", err))
		}
		removedLegacy = true
	}
	if removedLegacy && errors.Is(currentErr, errServiceNotInstalled) {
		return true, nil
	}
	return removedLegacy, currentErr
}

func runServiceStatus(cmd *cobra.Command, _ []string) error {
	user, _ := cmd.Flags().GetBool("user")
	if err := statusPrivilegeError(runtime.GOOS, user, os.Geteuid()); err != nil {
		return err
	}
	mgr, err := newServiceManager()
	if err != nil {
		return err
	}
	return mgr.status(user)
}

// statusPrivilegeError explains that launchctl's legacy list only shows
// system services to root.
func statusPrivilegeError(goos string, user bool, euid int) error {
	if goos == "darwin" && !user && euid != 0 {
		return errors.New("launchctl lists system services only to root; run: sudo runner service status --user=false")
	}
	return nil
}

// launchdServiceLogFile prefers the current stderr log and falls back to the
// combined log of plists generated by older releases.
func launchdServiceLogFile(user bool, exists func(string) bool) (path, note string) {
	if current := launchdStderrPath(user); exists(current) {
		return current, ""
	}
	if legacy := launchdLegacyLogPath(user); exists(legacy) {
		return legacy, fmt.Sprintf("  ⚠ Showing %s from a service generated by an older runner; regenerate it with 'runner service install --force' to log to %s", legacy, launchdStderrPath(user))
	}
	return "", ""
}
```

`runServiceStart` 與 `runServiceRestart` 在 `checkPrivileges` 之前分別加入 `refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "start the runner service", pathExists)` 與 `refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "restart the runner service", pathExists)`（錯誤時直接回傳）。

`removeSystemdUnit` 的未安裝錯誤改為 `fmt.Errorf("%w (no unit file at %s)", errServiceNotInstalled, unitPath)`；`removeLaunchdPlist` 改為 `fmt.Errorf("%w (no plist at %s)", errServiceNotInstalled, plistPath)`。

`systemdManager.install` 的既有安裝檢查、寫檔與啟動改為：

```go
	_, statErr := os.Stat(unitPath)
	existed := statErr == nil
	if existed && !opts.force {
		return fmt.Errorf("service already installed at %s\n\n  Regenerate it in place with --force", unitPath)
	}

	unit, err := renderSystemdUnit(opts)
	if err != nil {
		return fmt.Errorf("failed to render unit template: %w", err)
	}
	if err := writeFileAtomic(unitPath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("failed to write unit file: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", unitPath)

	userFlag := systemdUserFlag(opts.user)
	if err := runCmd("systemctl", append(userFlag, "daemon-reload")...); err != nil {
		return fmt.Errorf("systemctl daemon-reload failed: %w", err)
	}
	if err := runCmd("systemctl", append(userFlag, "enable", serviceName)...); err != nil {
		return fmt.Errorf("systemctl enable failed: %w", err)
	}
	fmt.Printf("  ✓ Service enabled\n")

	if !opts.noStart {
		action := "start"
		if existed {
			action = "restart"
		}
		if err := runCmd("systemctl", append(userFlag, action, serviceName)...); err != nil {
			return fmt.Errorf("systemctl %s failed: %w", action, err)
		}
		fmt.Printf("  ✓ Service %sed\n", action)
	}
```

`launchdManager.install` 的既有安裝檢查、寫檔與載入改為：

```go
	_, statErr := os.Stat(plist)
	existed := statErr == nil
	if existed && !opts.force {
		return fmt.Errorf("service already installed at %s\n\n  Regenerate it in place with --force", plist)
	}

	rendered, err := renderLaunchdPlist(opts)
	if err != nil {
		return fmt.Errorf("failed to render plist template: %w", err)
	}
	if err := writeFileAtomic(plist, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("failed to write plist file: %w", err)
	}
	fmt.Printf("  ✓ Service file installed at %s\n", plist)

	if !opts.noStart {
		if existed {
			_ = runCmd("launchctl", "unload", plist)
		}
		if err := runCmd("launchctl", "load", "-w", plist); err != nil {
			return fmt.Errorf("launchctl load failed: %w", err)
		}
		fmt.Printf("  ✓ Service loaded and started\n")
	}
```

`launchdManager.logs` 改為：

```go
func (m *launchdManager) logs(user bool, follow bool, lines int) error {
	logFile, note := launchdServiceLogFile(user, pathExists)
	if logFile == "" {
		return fmt.Errorf("service log not found at %s", launchdStderrPath(user))
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	args := []string{"-n", fmt.Sprintf("%d", lines)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, logFile)
	return runCmdPassthrough("tail", args...)
}
```

import 補 `"errors"`、`"io/fs"`。`detectProvider` 與 `detectDrainTimeout` 暫時保留給 migrate 使用，Task 10 移除。

- [ ] **Step 4: 確認測試通過**

Run: `go test ./cmd/runner -count=1`
Expected: PASS

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add cmd/runner/cmd_service.go cmd/runner/cmd_service_test.go cmd/runner/legacy.go cmd/runner/cmd_migrate.go
git commit -m "feat(service): validate config and binaries, then regenerate services in place with --force"
```

---

### Task 10: migrate 共用安裝流程、備份位置與 macOS 限制

**Files:**
- Modify: `cmd/runner/cmd_migrate.go`
- Modify: `cmd/runner/cmd_migrate_test.go`
- Modify: `cmd/runner/cmd_service.go`（移除 `detectProvider`、`detectDrainTimeout`）
- Modify: `cmd/runner/cmd_service_test.go`（兩個測試改測 `readServiceConfig`）

**Interfaces:**
- Consumes: `layout.CurrentIdentity`、`layout.For`（Task 1）；`refuseDarwinRoot`、`pathExists`（Task 7）；`buildInstallOpts`、`installService`、`validateServiceBinaryFor`、`readServiceConfig`（Task 9）
- Produces:
  - `migrationServiceDeps` 移除 `evalSymlinks`、`detectProvider`、`detectDrain`，新增 `prepareInstall func(user bool, configPath, binaryPath string) (installOpts, error)` 與 `validateBinary func(user bool, path string) (string, error)`
  - `func migrateScopeError(goos string, user bool) error`

- [ ] **Step 1: 確認舊函式只剩 migrate 與測試使用**

Run: `grep -rn --include='*.go' -E 'detectProvider|detectDrainTimeout' cmd`
Expected: 只出現在 `cmd_service.go` 的定義、`cmd_migrate.go` 的 `defaultMigrationServiceDeps`、兩個測試檔

- [ ] **Step 2: 寫失敗測試**

`cmd/runner/cmd_service_test.go`：把 `TestDetectProviderSupportsCanonicalAndLegacyKeys` 與 `TestDetectDrainTimeoutPreservesUnsetAndExplicitZero` 改為：

```go
func TestReadServiceConfigProviderSupportsCanonicalAndLegacyKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "canonical tart", body: `provider = "tart"`, want: "tart"},
		{name: "legacy tart", body: `backend = "tart"`, want: "tart"},
		{name: "mixed providers need docker service", body: `
[[scaleset]]
provider = "tart"
[[scaleset]]
provider = "docker"
`, want: "docker"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			facts, err := readServiceConfig(path, true)
			if err != nil || facts.provider != tt.want {
				t.Errorf("provider = %q, %v; want %q", facts.provider, err, tt.want)
			}
		})
	}
}

func TestReadServiceConfigPreservesUnsetAndExplicitZeroDrain(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	unset, err := readServiceConfig(write("unset.toml", `name = "runner"`), true)
	if err != nil || unset.drainTimeout != nil {
		t.Fatalf("omitted drain-timeout = %v, %v; want nil", unset.drainTimeout, err)
	}
	zero, err := readServiceConfig(write("zero.toml", `drain-timeout = "0s"`), true)
	if err != nil || zero.drainTimeout == nil || *zero.drainTimeout != 0 {
		t.Fatalf("explicit zero = %v, %v; want non-nil zero", zero.drainTimeout, err)
	}
}
```

`cmd/runner/cmd_migrate_test.go`：
- `newMigrationServiceTestDeps` 刪除 `evalSymlinks`、`detectProvider`、`detectDrain` 三行，加入：

```go
		prepareInstall: func(user bool, configPath, binaryPath string) (installOpts, error) {
			return installOpts{user: user, configPath: configPath, binaryPath: binaryPath, provider: "docker"}, nil
		},
		validateBinary: func(_ bool, path string) (string, error) { return path, nil },
```

- `TestResolveConfigMigrationPathsUsesUserOwnedDefaults`：開頭加 `t.Setenv("XDG_STATE_HOME", "")`，備份期望改為 `filepath.Join(home, ".local", "state", "runner", "backups")`
- 新增：

```go
func TestMigrateServiceStopsWhenBinaryIsUnsafe(t *testing.T) {
	var actions []string
	legacyInstalled, legacyRunning := true, true
	newInstalled, newRunning := false, false
	manager := &fakeMigrationServiceManager{actions: &actions, installed: &newInstalled, running: &newRunning}
	deps := newMigrationServiceTestDeps(manager, &actions, &legacyInstalled, &legacyRunning, &newInstalled, &newRunning)
	deps.validateBinary = func(bool, string) (string, error) {
		return "", errors.New("a system service must run a binary only root can modify")
	}

	_, cutover, err := migrateServiceWithDeps(false, "/etc/runner/config.toml", deps)
	if err == nil || cutover || len(actions) != 0 || !legacyRunning {
		t.Fatalf("err=%v cutover=%v actions=%v legacyRunning=%v; want refusal before touching services", err, cutover, actions, legacyRunning)
	}
}

func TestMigrateServiceUsesSharedInstallOptions(t *testing.T) {
	var actions []string
	legacyInstalled, legacyRunning := true, false
	newInstalled, newRunning := false, false
	manager := &fakeMigrationServiceManager{actions: &actions, installed: &newInstalled, running: &newRunning}
	deps := newMigrationServiceTestDeps(manager, &actions, &legacyInstalled, &legacyRunning, &newInstalled, &newRunning)
	logFile := "/var/log/custom/runner.log"
	deps.prepareInstall = func(user bool, configPath, binaryPath string) (installOpts, error) {
		return installOpts{user: user, configPath: configPath, binaryPath: binaryPath, logFile: &logFile, xdgStateHome: "/xdg/state"}, nil
	}

	if _, _, err := migrateServiceWithDeps(true, "/home/test/.config/runner/config.toml", deps); err != nil {
		t.Fatal(err)
	}
	got := manager.installOpts
	if got.logFile == nil || *got.logFile != logFile || got.xdgStateHome != "/xdg/state" || !got.noStart {
		t.Fatalf("install options = %+v, want the shared log file, XDG state and noStart", got)
	}
}

func TestMigrateScopeError(t *testing.T) {
	if err := migrateScopeError("darwin", false); err == nil || !strings.Contains(err.Error(), "runner migrate --user") {
		t.Errorf("macOS system migration = %v", err)
	}
	for _, tc := range []struct {
		goos string
		user bool
	}{{"darwin", true}, {"linux", false}, {"linux", true}} {
		if err := migrateScopeError(tc.goos, tc.user); err != nil {
			t.Errorf("migrateScopeError(%+v) = %v", tc, err)
		}
	}
}
```

- [ ] **Step 3: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestMigrate|TestResolveConfigMigrationPaths|TestReadServiceConfig'`
Expected: FAIL，`unknown field prepareInstall`、`undefined: migrateScopeError`

- [ ] **Step 4: 實作**

`cmd/runner/cmd_migrate.go`：
- `backup-dir` flag 說明改為 `"Directory for versioned config backups (default: /var/lib/runner/backups for system scope, $XDG_STATE_HOME/runner/backups for --user)"`
- 新增：

```go
// migrateScopeError rejects system-scope migration on macOS, where runner
// only runs as the logged-in user.
func migrateScopeError(goos string, user bool) error {
	if goos == "darwin" && !user {
		return errors.New("system-level migration is not supported on macOS: runner runs as the logged-in user.\n\n  Run 'runner migrate --user', then remove the old LaunchDaemon with 'sudo runner service uninstall --user=false'")
	}
	return nil
}
```

- `runMigrate` 在讀取旗標之後、`checkPrivileges` 之前加入：

```go
	if err := refuseDarwinRoot(runtime.GOOS, os.Geteuid(), "migrate the runner service", pathExists); err != nil {
		return err
	}
	if err := migrateScopeError(runtime.GOOS, user); err != nil {
		return err
	}
```

- `resolveConfigMigrationPaths` 的預設備份目錄（`paths.BackupDir = filepath.Join(filepath.Dir(paths.Target), "backups")`）改為：

```go
		id := layout.CurrentIdentity()
		// The migration scope, not the invoking user, owns the backups: a
		// --user=false dry run by a user still plans system locations.
		id.Root = !user
		lay, err := layout.For(id)
		if err != nil {
			return paths, fmt.Errorf("resolve backup directory: %w", err)
		}
		paths.BackupDir = lay.BackupDir
```

- `migrationServiceDeps` 刪除 `evalSymlinks`、`detectProvider`、`detectDrain`，加入：

```go
	prepareInstall func(user bool, configPath, binaryPath string) (installOpts, error)
	validateBinary func(user bool, path string) (string, error)
```

- `defaultMigrationServiceDeps` 刪除對應三行，加入：

```go
		prepareInstall: func(user bool, configPath, binaryPath string) (installOpts, error) {
			// Migration has already written and validated the target config.
			opts, warnings, err := buildInstallOpts(user, configPath, binaryPath, true, os.Getenv)
			for _, w := range warnings {
				warnLegacy("%s", w)
			}
			return opts, err
		},
		validateBinary: validateServiceBinaryFor,
```

- `migrateServiceWithDeps` 的安裝段落（從 `binaryPath, err := deps.executable()` 到 `installedNew = true`）改為：

```go
		binaryPath, err := deps.executable()
		if err != nil {
			return false, false, fmt.Errorf("cannot detect binary path: %w", err)
		}
		opts, err := deps.prepareInstall(user, configPath, binaryPath)
		if err != nil {
			return false, false, err
		}
		opts.noStart = true
		if err := installService(mgr, opts, deps.validateBinary); err != nil {
			// install may have written the service file before a daemon command
			// failed. Remove only the unit this attempt created.
			if deps.newInstalled(user) {
				_ = mgr.uninstall(user)
			}
			return false, false, err
		}
		installedNew = true
```

- import 補 `"github.com/ysya/runscaler/internal/layout"`

`cmd/runner/cmd_service.go`：刪除 `detectProvider` 與 `detectDrainTimeout` 兩個函式（`platformDefaultProvider` 保留，`readServiceConfig` 仍使用）。

- [ ] **Step 5: 確認測試通過**

Run: `go test ./cmd/runner -count=1`
Expected: PASS

- [ ] **Step 6: 全綠檢查並提交**

```bash
git add cmd/runner/cmd_migrate.go cmd/runner/cmd_migrate_test.go cmd/runner/cmd_service.go cmd/runner/cmd_service_test.go
git commit -m "feat(migrate): share service installation checks and keep backups in state directories"
```

---

### Task 11: `runner init` 寫到標準位置

**Files:**
- Modify: `cmd/runner/cmd_init.go`
- Modify: `cmd/runner/cmd_init_test.go`

**Interfaces:**
- Consumes: `layout.CurrentIdentity`、`layout.For`、`Identity.DirPerm`（Task 1）；`writeFileAtomic`、`sameFilePath`（`migrate_config.go`）；`reader`（`cmd_init.go`）；`assertFileMode`（`cmd_migrate_test.go`）
- Produces: `func initShadowWarning(output string) string`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/cmd_init_test.go`：把 `runInitTestConfig` 拆出可指定輸出的版本，並新增測試（import 補 `"bufio"`）：

```go
func runInitTestConfig(t *testing.T, providerFlag string) string {
	t.Helper()
	output := filepath.Join(t.TempDir(), "config.toml")
	runInitWithOutput(t, providerFlag, output)
	return output
}

func runInitWithOutput(t *testing.T, providerFlag, output string) {
	t.Helper()
	c := &cobra.Command{}
	f := c.Flags()
	f.String("output", "", "")
	f.String("url", "", "")
	f.String("name", "", "")
	f.String("token", "", "")
	f.Int("max-runners", 1, "")
	f.String("provider", "", "")
	f.String("backend", "", "")
	f.String("runner-image", "", "")
	f.Bool("dind", false, "")
	f.String("shared-volume", "", "")
	values := map[string]string{
		"url": "https://github.com/test-org", "name": "test-runners",
		"token": "fake-token", "max-runners": "1", providerFlag: "docker",
		"runner-image": "test-image", "dind": "false", "shared-volume": "",
	}
	if output != "" {
		values["output"] = output
	}
	for name, value := range values {
		if err := f.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInit(c, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInitDefaultsToUserConfigPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes /etc/runner/config.toml")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Chdir(t.TempDir())

	runInitWithOutput(t, "provider", "")

	path := filepath.Join(home, ".config", "runner", "config.toml")
	assertGeneratedConfigValid(t, path)
	assertFileMode(t, path, 0o600)
	assertFileMode(t, filepath.Dir(path), 0o700)
}

func TestInitRewritesExistingConfigPrivately(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(output, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := reader
	t.Cleanup(func() { reader = saved })
	reader = bufio.NewReader(strings.NewReader("y\n")) // confirm the overwrite prompt

	runInitWithOutput(t, "provider", output)

	assertFileMode(t, output, 0o600)
	assertGeneratedConfigValid(t, output)
}

func TestInitShadowWarning(t *testing.T) {
	t.Chdir(t.TempDir())
	central := filepath.Join(t.TempDir(), "config.toml")
	if got := initShadowWarning(central); got != "" {
		t.Fatalf("no local config but warning %q", got)
	}
	if err := os.WriteFile("config.toml", []byte("name = 'local'"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := initShadowWarning(central); !strings.Contains(got, central) || !strings.Contains(got, "config.toml") {
		t.Fatalf("shadow warning = %q", got)
	}
	if got := initShadowWarning("config.toml"); got != "" {
		t.Fatalf("writing the local file itself warned: %q", got)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestInit'`
Expected: FAIL，`undefined: initShadowWarning`；`TestInitDefaultsToUserConfigPath` 找不到檔案（仍寫到目前目錄）

- [ ] **Step 3: 實作**

`cmd/runner/cmd_init.go`：
- flag：`flags.String("output", "", "Output file path (default: /etc/runner/config.toml as root, $XDG_CONFIG_HOME/runner/config.toml otherwise)")`
- `runInit` 開頭改為：

```go
func runInit(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	id := layout.CurrentIdentity()
	if output == "" {
		lay, err := layout.For(id)
		if err != nil {
			return fmt.Errorf("resolve default config path: %w", err)
		}
		output = lay.ConfigFile
	}
```

- 寫檔段落（`os.WriteFile(output, ...)` 到 `fmt.Printf("\nCreated %s\n", output)`）改為：

```go
	if err := os.MkdirAll(filepath.Dir(output), id.DirPerm()); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(output), err)
	}
	// Atomic replace creates a new 0600 file: WriteFile would keep an existing
	// file's looser mode and follow a symlink at the destination.
	if err := writeFileAtomic(output, []byte(configContent), 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", output, err)
	}

	fmt.Printf("\nCreated %s\n", output)
	if warning := initShadowWarning(output); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
```

- 兩處範本註解 `# log-file defaults to runner.log beside this config; set log-file = "" to disable` 改為：

```
# log-file defaults to /var/log/runner/runner.log (root), ~/.local/state/runner/runner.log (Linux) or ~/Library/Logs/runner/runner.log (macOS); set log-file = "" to disable
```

- 新增：

```go
// initShadowWarning explains when ./config.toml would be read instead of the
// file init just wrote, because config search checks the working directory
// first.
func initShadowWarning(output string) string {
	local, err := filepath.Abs("config.toml")
	if err != nil || sameFilePath(local, output) {
		return ""
	}
	if _, err := os.Stat(local); err != nil {
		return ""
	}
	written, _ := filepath.Abs(output)
	return fmt.Sprintf("  ⚠ %s is found before %s when --config is omitted; pass --config %s or remove the local file", local, written, written)
}
```

- import 補 `"path/filepath"`、`"github.com/ysya/runscaler/internal/layout"`

- [ ] **Step 4: 確認測試通過**

Run: `go test ./cmd/runner -count=1`
Expected: PASS

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add cmd/runner/cmd_init.go cmd/runner/cmd_init_test.go
git commit -m "feat(init): write new configs to the standard config path"
```

---

### Task 12: `runner logs` 以開啟後的 fd 檢查 regular file

**Files:**
- Modify: `cmd/runner/cmd_logs.go`
- Modify: `cmd/runner/cmd_logs_test.go`

**Interfaces:**
- Consumes: `resolveLogFile`（Task 5）；`layout.SystemLogFile`、`layout.Identity`（Task 1）
- Produces:
  - `func openRegularFile(path string) (*os.File, error)`
  - `func explainLogReadError(err error, path string, id layout.Identity) error`
  - `tailFile(cmd *cobra.Command, path string, lines int, follow bool) error` 簽名不變，改用 `openRegularFile`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/cmd_logs_test.go` 追加（import 補 `"errors"`、`"io"`、`"io/fs"`、`"syscall"`、`"time"`、`"github.com/ysya/runscaler/internal/layout"`）：

```go
func TestOpenRegularFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openRegularFile(path)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("openRegularFile(FIFO) = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}

func TestOpenRegularFileRejectsDevices(t *testing.T) {
	f, err := openRegularFile(os.DevNull)
	if f != nil {
		_ = f.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("openRegularFile(%s) = %v", os.DevNull, err)
	}
}

func TestTailFileFollowStopsWhenLogBecomesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &cobra.Command{}
	c.SetOut(io.Discard)
	c.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- tailFile(c, path, 1, true) }()

	time.Sleep(100 * time.Millisecond) // let the initial read finish
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("tailFile() = %v, want a refusal to read the FIFO", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follow kept waiting on a FIFO")
	}
}

func TestExplainLogReadError(t *testing.T) {
	missing := explainLogReadError(&fs.PathError{Op: "open", Path: "/x/runner.log", Err: fs.ErrNotExist}, "/x/runner.log", layout.Identity{Root: true})
	if !strings.Contains(missing.Error(), "/x/runner.log") {
		t.Errorf("missing file error = %v", missing)
	}
	denied := explainLogReadError(&fs.PathError{Op: "open", Path: "/var/log/runner/runner.log", Err: fs.ErrPermission}, "/var/log/runner/runner.log", layout.Identity{Home: "/home/ada"})
	if !strings.Contains(denied.Error(), "sudo runner logs") || !errors.Is(denied, fs.ErrPermission) {
		t.Errorf("permission error = %v", denied)
	}
	other := errors.New("boom")
	if got := explainLogReadError(other, "/x/runner.log", layout.Identity{}); got != other {
		t.Errorf("unrelated error = %v, want it unchanged", got)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestOpenRegularFile|TestTailFileFollow|TestExplainLogReadError'`
Expected: FAIL，`undefined: openRegularFile`、`undefined: explainLogReadError`

- [ ] **Step 3: 實作**

`cmd/runner/cmd_logs.go`：`runLogs` 結尾的 `tailFile` 錯誤處理改為：

```go
	if err := tailFile(cmd, path, lines, follow); err != nil {
		return explainLogReadError(err, path, id)
	}
	return nil
```

`tailFile` 整份改為：

```go
func tailFile(cmd *cobra.Command, path string, lines int, follow bool) error {
	f, err := openRegularFile(path)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	parts := strings.Split(string(data), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	start := max(0, len(parts)-lines)
	if lines > 0 && start < len(parts) {
		fmt.Fprintln(cmd.OutOrStdout(), strings.Join(parts[start:], "\n"))
	}
	if !follow {
		return nil
	}

	offset := int64(len(data))
	identity, err := os.Stat(path)
	if err != nil {
		return err
	}
	for {
		select {
		case <-cmd.Context().Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file; refusing to read it", path)
		}
		if !os.SameFile(identity, info) || info.Size() < offset {
			offset = 0
			identity = info
		}
		if info.Size() == offset {
			continue
		}
		f, err := openRegularFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			fmt.Fprintln(cmd.OutOrStdout(), scanner.Text())
		}
		offset, _ = f.Seek(0, io.SeekCurrent)
		scanErr := scanner.Err()
		_ = f.Close()
		if scanErr != nil {
			return scanErr
		}
	}
}

// openRegularFile opens path for reading and refuses anything but a regular
// file, checked on the opened descriptor so a swapped path cannot slip in.
// O_NONBLOCK keeps a FIFO from blocking the open itself.
func openRegularFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file; refusing to read it", path)
	}
	return f, nil
}

// explainLogReadError turns the common failures into the next command to try.
func explainLogReadError(err error, path string, id layout.Identity) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		msg := fmt.Sprintf("log file %s does not exist — runner may not have started as this user yet", path)
		if !id.Root && path != layout.SystemLogFile {
			if _, sysErr := os.Stat(layout.SystemLogFile); sysErr == nil || errors.Is(sysErr, fs.ErrPermission) {
				msg += fmt.Sprintf("\n\n  A system service logs to %s; run: sudo runner logs", layout.SystemLogFile)
			}
		}
		return errors.New(msg)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w\n\n  run: sudo runner logs", err)
	}
	return err
}
```

import 補 `"io/fs"`、`"golang.org/x/sys/unix"`（`bufio`、`errors`、`fmt`、`io`、`os`、`strings`、`time`、`layout` 已存在）。

- [ ] **Step 4: 確認測試通過**

Run: `go test ./cmd/runner -count=1 -run 'TestTailFile|TestOpenRegularFile|TestExplainLogReadError' -race`
Expected: PASS

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add cmd/runner/cmd_logs.go cmd/runner/cmd_logs_test.go
git commit -m "fix(logs): read only regular files and re-check them while following"
```

---

### Task 13: `runner update` 的 sudo 提示與 install.sh

**Files:**
- Modify: `cmd/runner/cmd_update.go`
- Modify: `cmd/runner/cmd_update_test.go`
- Modify: `install.sh`

**Interfaces:**
- Consumes: 無
- Produces: `func updateFailure(err error, execPath string) error`

- [ ] **Step 1: 寫失敗測試**

`cmd/runner/cmd_update_test.go` 追加（import 補 `"fmt"`、`"io/fs"`、`"syscall"`；`errors`、`strings` 已存在）：

```go
func TestUpdateFailureSuggestsSudoOnPermissionErrors(t *testing.T) {
	denied := fmt.Errorf("failed to create temp file in /usr/local/bin: %w",
		&fs.PathError{Op: "open", Path: "/usr/local/bin/.runner-update-1", Err: syscall.EACCES})
	if err := updateFailure(denied, "/usr/local/bin/runner"); !strings.Contains(err.Error(), "sudo runner update") || !errors.Is(err, fs.ErrPermission) {
		t.Errorf("permission failure = %v", err)
	}
	if err := updateFailure(errors.New("checksum mismatch"), "/usr/local/bin/runner"); strings.Contains(err.Error(), "sudo") {
		t.Errorf("unrelated failure suggested sudo: %v", err)
	}
}
```

- [ ] **Step 2: 確認測試失敗**

Run: `go test ./cmd/runner -count=1 -run 'TestUpdateFailure'`
Expected: FAIL，`undefined: updateFailure`

- [ ] **Step 3: 實作**

`cmd/runner/cmd_update.go` 的 `versioncheck.Update` 錯誤處理改為：

```go
	if err := versioncheck.Update(cmd.Context(), release.TagName, execPath); err != nil {
		return updateFailure(err, execPath)
	}
```

並新增（import 補 `"errors"`、`"io/fs"`、`"path/filepath"`）：

```go
// updateFailure adds a sudo hint when the binary lives in a directory only
// root can write, as a system service's binary must.
func updateFailure(err error, execPath string) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("update failed: %w\n\n  %s is not writable by this user; run: sudo runner update", err, filepath.Dir(execPath))
	}
	return fmt.Errorf("update failed: %w", err)
}
```

`install.sh` 的 `INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"` 改為：

```sh
if [ "$(id -u)" -eq 0 ]; then
    # A binary run by a root service must be writable only by root.
    DEFAULT_INSTALL_DIR="/usr/local/bin"
else
    DEFAULT_INSTALL_DIR="${HOME}/.local/bin"
fi
INSTALL_DIR="${INSTALL_DIR:-${DEFAULT_INSTALL_DIR}}"
```

PATH 提示的 export 行改為 `echo "  export PATH=\"${INSTALL_DIR}:\${PATH}\""`。

- [ ] **Step 4: 確認測試與腳本**

Run: `go test ./cmd/runner -count=1 -run 'TestUpdate' && sh -n install.sh`
Expected: PASS；`sh -n` 無輸出

Run: `for u in 0 1000; do tmp=$(mktemp -d); printf '#!/bin/sh\necho %s\n' "$u" > "$tmp/id"; chmod +x "$tmp/id"; env -u INSTALL_DIR HOME=/home/test PATH="$tmp:$PATH" sh -c 'sed -n "/^if \[ \"\$(id -u)\"/,/^INSTALL_DIR=/p" install.sh > '"$tmp"'/snippet.sh; . '"$tmp"'/snippet.sh; echo "$INSTALL_DIR"'; done`
Expected: 依序輸出 `/usr/local/bin` 與 `/home/test/.local/bin`

- [ ] **Step 5: 全綠檢查並提交**

```bash
git add cmd/runner/cmd_update.go cmd/runner/cmd_update_test.go install.sh
git commit -m "feat(install): default root installs to /usr/local/bin and hint sudo updates"
```

---

### Task 14: 文件

**Files:**
- Modify: `README.md`
- Modify: `config.example.toml:12`

**Interfaces:**
- Consumes: 前述所有行為
- Produces: 無

- [ ] **Step 1: Quick Start 的「Run」段**

把 `### Run` 底下程式碼區塊中的四行：

```bash
# Generate config interactively
runner init

# Validate everything before starting
runner validate --config config.toml

# Start scaling
runner run --config config.toml
```

改為：

```bash
# Generate config interactively
# (writes ~/.config/runner/config.toml, or /etc/runner/config.toml as root)
runner init

# Validate everything before starting (the standard config path is found automatically)
runner validate

# Start scaling
runner run
```

同一區塊最後的 `runner run --dry-run --config config.toml` 改為 `runner run --dry-run`。

- [ ] **Step 2: 「Logs」段**

把 `### Logs` 的第一段（「`runner run` writes to stdout and a rotating file…」到「…set `log-file = ""` to disable file logging.」）替換為：

```markdown
`runner run` writes to stdout and a rotating file (10 MB plus one backup). The
file's default location depends on who runs it:

| Runs as | Log file |
| --- | --- |
| root (system service or `sudo runner run`) | `/var/log/runner/runner.log` |
| a user on Linux | `$XDG_STATE_HOME/runner/runner.log` (`~/.local/state/runner/runner.log`) |
| a user on macOS | `~/Library/Logs/runner/runner.log` |

Set `log-file` to an explicit path, or `log-file = ""` to disable file logging.
As root, `log-file` must be in a directory under `/var/log` whose name uses only
letters, digits, `.`, `_` or `-`; anything else falls back to the default with a
warning. Releases before v0.10 wrote `runner.log` beside the config; that file is
left in place.
```

保留色彩說明段落。其後的範例改為：

```bash
runner logs                 # last 100 lines (add sudo for a system service)
runner logs -n 500 -f
```

- [ ] **Step 3: 設定範例中的 `log-file` 註解**

README 設定範例中的 `# log-file = "/var/log/runner/runner.log" # default: runner.log beside config` 與 `config.example.toml` 第 12 行，都改為：

```toml
# log-file = "/var/log/runner/runner.log" # default: /var/log/runner/runner.log (root), ~/.local/state/runner/runner.log (Linux), ~/Library/Logs/runner/runner.log (macOS); "" disables
```

- [ ] **Step 4: 「Graceful Shutdown」的重裝說明**

把「`runner service install` generates a systemd `TimeoutStopSec`…」起，到「…so a Homebrew-installed `tart` is found without editing the plist.」為止的整段替換為：

````markdown
`runner service install` generates a systemd `TimeoutStopSec` or launchd
`ExitTimeOut` one minute longer than the configured drain budget and records
it, so runner warns when a later `drain-timeout` outgrows it. A binary update
does not rewrite an installed service definition; regenerate it in place:

```bash
sudo runner service install --user=false --force --config-path /etc/runner/config.toml   # Linux system service
runner service install --force --config-path ~/.config/runner/config.toml               # user service
```

`--force` validates the config and binary and renders the new definition before
touching the installed one. When a definition is outdated, runner logs this
command with the exact config and binary paths it is running with.

After that, the normal upgrade flow is:

```bash
runner update              # add sudo when the binary is in /usr/local/bin
runner service restart     # waits for active jobs automatically
```
````

- [ ] **Step 5: 新增「Service layout」段（放在 `### Systemd` 之前）**

```markdown
### Service layout

- **Linux system service** runs as root and must execute a binary only root can
  modify, such as `/usr/local/bin/runner`; `runner service install --user=false`
  refuses anything else. The unit sets `ProtectSystem=strict`: `/etc/runner`
  stays read-only, logs go to `LogsDirectory=runner` (`/var/log/runner`), and
  only `/tmp` is writable, for the run lock.
- **macOS** supports only a LaunchAgent for the logged-in user. It starts at
  login, so enable automatic login on unattended hosts, and install it from a
  local GUI session rather than SSH. runner refuses to run, install or migrate
  as root on macOS. launchd starts agents with a minimal `PATH`; runner appends
  `/opt/homebrew/bin` and `/usr/local/bin` when present so a Homebrew-installed
  `tart` is found. The agent discards stdout (runner writes its own rotated log)
  and keeps stderr in `~/Library/Logs/runner/stderr.log`, which is not rotated.
- The run lock at `/tmp/runner.lock` prevents accidentally starting a second
  runner; it is not a security boundary against local users.
```

- [ ] **Step 6: 新增「Upgrading to v0.10」段（放在 `## Upgrading from runscaler` 之前）**

```markdown
## Upgrading to v0.10

v0.10 moves runner's own files to the platform's standard locations and never
moves existing files.

| Existing setup | After upgrading the binary | What to do |
| --- | --- | --- |
| Linux system service | Fails to take the lock and prints a fix | Put the binary in `/usr/local/bin` (root-owned), then run the printed `service install --user=false --force` command |
| Linux user service | Runs; logs move; warns | Run the printed `service install --force` command |
| macOS LaunchAgent | Runs; logs move; warns | Run the printed `service install --force` command |
| macOS LaunchDaemon | Refuses to run as root | Convert it as below |
| tmux / foreground | Logs move; notes the old file | Nothing; delete the old `runner.log` to silence the note |

### macOS LaunchDaemon to LaunchAgent

1. Record the old definition: `sudo cat /Library/LaunchDaemons/io.github.ysya.runner.plist`
   (or `com.runscaler.agent.plist`) and note the binary and config paths in
   `ProgramArguments`.
2. Remove it: `sudo runner service uninstall --user=false` (this also removes
   the legacy plist).
3. Hand the config to your user:
   `mkdir -p ~/.config/runner && chmod 700 ~/.config/runner`, then
   `sudo install -o "$USER" -m 0600 <old config> ~/.config/runner/config.toml`
4. Tart images used by root live in `/var/root/.tart`; pull them again as your
   user (and update `[tart] home` if it pointed at root's directory).
5. Keep the binary where your user can run it (`~/.local/bin`, or
   `/usr/local/bin` with `sudo runner update`).
6. Install as your user from a local GUI session:
   `runner service install --config-path ~/.config/runner/config.toml`
7. Enable automatic login so the agent starts after a reboot.
```

- [ ] **Step 7: 「Upgrading from runscaler」中的過時說明**

- 「System backups live under `/etc/runner/backups`; `--user` backups live under `~/.config/runner/backups`.」改為「System backups live under `/var/lib/runner/backups`; `--user` backups live under `$XDG_STATE_HOME/runner/backups` (`~/.local/state/runner/backups`).」
- 「On macOS, `runner migrate` and all `runner service` commands default to user scope (no sudo).」之後補一句：「On macOS, `runner migrate` refuses system scope and refuses to run as root; migrate with `--user`.」
- 從「On both Linux and macOS, an absolute, nonempty `XDG_CONFIG_HOME` replaces」起，到「…`--config-path` overrides `--config` for service installation.」為止的整段替換為：

```markdown
On both Linux and macOS, an absolute, nonempty `XDG_CONFIG_HOME` replaces
`~/.config` for user config, and `XDG_STATE_HOME` replaces `~/.local/state` for
user logs (Linux) and backups. Relative values are ignored. Installing a user
service records these variables in the service definition so the service and
the CLI agree. Config loading searches the current directory, user runner
directory, `/etc/runner`, user runscaler directory, then `/etc/runscaler`. An
explicit `--config` takes precedence. `init` writes the standard config path
for the current user unless `--output` is supplied, and warns when a
`config.toml` in the current directory would be found first. User service
installation selects that local file if present, otherwise the user runner
config; it stores an absolute path in the service definition. `--config-path`
overrides `--config` for service installation.
```

- [ ] **Step 8: Commands 表新增 service 列**

在 `| runner logs |` 列之後加入：

```markdown
| `runner service`         | Install, regenerate (`--force`), start, stop, and inspect the service |
```

- [ ] **Step 9: 檢查殘留的舊說法並確認全綠**

Run: `grep -n -E -- '--config config\.toml|beside (this )?config|/etc/runner/backups|\.config/runner/backups|continues to write' README.md config.example.toml; grep -n -E 'beside (this )?config' cmd/runner/*.go`
Expected: 無輸出（cobra 說明裡明確指定檔案的 `--config config.toml` 範例仍然正確，不在檢查範圍）

Run: 全綠指令

- [ ] **Step 10: 提交**

```bash
git add README.md config.example.toml
git commit -m "docs: document the per-identity layout and upgrade steps"
```

---

### Task 15: 端對端驗證

**Files:**
- 無程式碼變更；所有暫存檔放在 scratchpad（下稱 `$SP`）

**Interfaces:**
- Consumes: 全部前述 task 的 binary 行為
- Produces: 驗證紀錄。任何 Step 不符 Expected 時，回到對應 task 以 TDD 修正後重跑本 task

每個 Step 的指令都以 `set -e` 執行；容器內的等待一律透過 `wait-for <秒數> <條件>`，逾時即以非 0 結束。

- [ ] **Step 1: 建立映像、容器與測試資料**

```bash
set -e
mkdir -p "$SP/e2e/image" "$SP/e2e/bin" "$SP/e2e/v091"
docker image inspect debian:12 >/dev/null 2>&1 && echo 1 > "$SP/e2e/debian-preexisted" || echo 0 > "$SP/e2e/debian-preexisted"
cat > "$SP/e2e/image/Dockerfile" <<'EOF'
FROM debian:12
RUN apt-get update \
 && apt-get install -y --no-install-recommends systemd systemd-sysv dbus netcat-openbsd procps \
 && apt-get clean && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
EOF
cat > "$SP/e2e/wait-for" <<'EOF'
#!/bin/sh
# usage: wait-for SECONDS CONDITION...
deadline=$(( $(date +%s) + $1 )); shift
until sh -c "$*"; do
  if [ "$(date +%s)" -ge "$deadline" ]; then echo "timed out waiting for: $*" >&2; exit 1; fi
  sleep 0.2
done
EOF
docker build -q -t runner-systemd-e2e:local "$SP/e2e/image"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$SP/e2e/bin/runner" ./cmd/runner
gh release download v0.9.1 -R ysya/runscaler -p 'runner-linux-arm64.tar.gz' -D "$SP/e2e/v091" --clobber
tar -xzf "$SP/e2e/v091/runner-linux-arm64.tar.gz" -C "$SP/e2e/v091"
printf 'provider = "tart"\nmax-runners = 1\nurl = "https://github.com/example-org"\nname = "probe"\ntoken = "ghp_dummy"\n' > "$SP/e2e/config.toml"
docker run -d --name runner-e2e --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw runner-systemd-e2e:local
docker cp -q "$SP/e2e/wait-for" runner-e2e:/usr/local/bin/wait-for
docker exec runner-e2e sh -c 'chmod 0755 /usr/local/bin/wait-for && wait-for 60 "systemctl is-system-running | grep -qE \"running|degraded\"" && mkdir -p /etc/runner && useradd -m probe'
docker cp -q "$SP/e2e/config.toml" runner-e2e:/etc/runner/config.toml
```

Expected: 全部指令成功；映像能 build、容器內 systemd 就緒（arm64 主機；x86 主機改用 `GOARCH=amd64` 與 `runner-linux-amd64.tar.gz`）

- [ ] **Step 2: 新裝（root 擁有的 binary）**

```bash
set -e
docker cp -q "$SP/e2e/bin/runner" runner-e2e:/usr/local/bin/runner
docker exec runner-e2e sh -c 'chown root:root /usr/local/bin/runner && chmod 0755 /usr/local/bin/runner'
docker exec runner-e2e /usr/local/bin/runner service install --user=false --config-path /etc/runner/config.toml
docker exec runner-e2e systemd-analyze verify /etc/systemd/system/runner.service
docker exec runner-e2e sh -c '
  set -e
  systemctl cat runner | grep -qx "ReadWritePaths=/tmp"
  systemctl cat runner | grep -qx "LogsDirectory=runner"
  wait-for 30 "journalctl -u runner -o cat | grep -q \"tart binary not found\""
  if journalctl -u runner -o cat | grep -q "runner lock"; then exit 1; fi
  if journalctl -u runner -o cat | grep -q "^Usage:"; then exit 1; fi
  wait-for 30 "test -s /var/log/runner/runner.log"
  test "$(stat -c "%U %a" /tmp/runner.lock)" = "root 666"
  echo fresh-install-ok'
```

Expected: 輸出 `fresh-install-ok`（`systemd-analyze verify` 對 unit 無錯誤；service 越過 lock、失敗原因是 Linux 上沒有 tart）

- [ ] **Step 3: docker socket 在唯讀路徑上仍可連線**

```bash
set -e
docker exec runner-e2e sh -c '
  set -e
  rm -f /run/probe.sock /tmp/probe.out
  (nc -lU /run/probe.sock > /tmp/probe.out &)
  wait-for 10 "test -S /run/probe.sock"
  systemd-run --wait --pipe -p ProtectSystem=strict -p ReadWritePaths=/tmp sh -c "echo hello | nc -U -N /run/probe.sock"
  wait-for 10 "grep -qx hello /tmp/probe.out"
  echo socket-ok'
```

Expected: 輸出 `socket-ok`。若失敗：回到 Task 8，docker provider 時在 `ReadWritePaths` 加入 `-/var/run/docker.sock` 並補測試

- [ ] **Step 4: 舊 unit + 新 binary 的升級提示與 `--force`**

```bash
set -e
docker exec runner-e2e /usr/local/bin/runner service uninstall --user=false
docker cp -q "$SP/e2e/v091/runner" runner-e2e:/usr/local/bin/runner
docker exec runner-e2e sh -c 'chown root:root /usr/local/bin/runner && /usr/local/bin/runner service install --user=false --config-path /etc/runner/config.toml && systemctl stop runner'
docker cp -q "$SP/e2e/bin/runner" runner-e2e:/usr/local/bin/runner
docker exec runner-e2e sh -c 'chown root:root /usr/local/bin/runner && chmod 0755 /usr/local/bin/runner && systemctl start runner'
docker exec runner-e2e wait-for 30 'journalctl -u runner -o cat | grep -q -- "--force"'
FIX=$(docker exec runner-e2e sh -c 'journalctl -u runner -o cat | grep -o "sudo .*--force.*" | tail -1')
echo "$FIX"
case "$FIX" in *"--config-path '/etc/runner/config.toml' --binary-path '/usr/local/bin/runner'"*) ;; *) echo "fix command lacks the real paths" >&2; exit 1;; esac
docker exec runner-e2e sh -c "${FIX#sudo }"   # the container runs as root and has no sudo
docker exec runner-e2e sh -c 'systemctl cat runner | grep -qx "ReadWritePaths=/tmp" && echo upgrade-ok'
```

Expected: 印出含實際路徑的指令，最後輸出 `upgrade-ok`

- [ ] **Step 5: 驗證失敗時 `--force` 不動既有 unit**

```bash
set -e
docker exec runner-e2e sh -c '
  set -e
  sha256sum /etc/systemd/system/runner.service > /tmp/unit.sum
  install -o probe -m 0755 /usr/local/bin/runner /home/probe/runner
  if /usr/local/bin/runner service install --user=false --force --config-path /etc/runner/config.toml --binary-path /home/probe/runner 2>/tmp/unsafe.err; then exit 1; fi
  grep -q "sudo install -m 0755" /tmp/unsafe.err
  printf "provider = \n" > /tmp/broken.toml
  if /usr/local/bin/runner service install --user=false --force --config-path /tmp/broken.toml 2>/tmp/broken.err; then exit 1; fi
  sha256sum -c /tmp/unit.sum
  echo refusal-ok'
```

Expected: 兩次安裝都失敗、unit 雜湊 `OK`，最後輸出 `refusal-ok`

- [ ] **Step 6: 跨身分 lock**

```bash
set -e
docker exec runner-e2e sh -c '
  set -e
  systemctl stop runner
  rm -f /tmp/runner.lock
  cp /etc/runner/config.toml /home/probe/config.toml && chown probe /home/probe/config.toml

  # root creates the lock, then a user runs
  /usr/local/bin/runner run --config /etc/runner/config.toml >/tmp/root1.out 2>&1 || true
  grep -q "tart binary not found" /tmp/root1.out
  test "$(stat -c %U /tmp/runner.lock)" = root
  su probe -c "/usr/local/bin/runner run --config /home/probe/config.toml" >/tmp/user1.out 2>&1 || true
  grep -q "tart binary not found" /tmp/user1.out
  if grep -q "runner lock" /tmp/user1.out; then exit 1; fi

  # a user creates the lock, then root runs with protected_regular enabled
  rm -f /tmp/runner.lock
  orig=$(sysctl -n fs.protected_regular)
  trap "sysctl -qw fs.protected_regular=$orig" EXIT
  sysctl -qw fs.protected_regular=1
  su probe -c "/usr/local/bin/runner run --config /home/probe/config.toml" >/tmp/user2.out 2>&1 || true
  grep -q "tart binary not found" /tmp/user2.out
  test "$(stat -c %U /tmp/runner.lock)" = probe
  /usr/local/bin/runner run --config /etc/runner/config.toml >/tmp/root2.out 2>&1 || true
  grep -q "tart binary not found" /tmp/root2.out
  if grep -q "runner lock" /tmp/root2.out; then exit 1; fi
  echo cross-identity-ok'
```

Expected: 輸出 `cross-identity-ok`（四次執行都越過 lock 並停在 tart 錯誤；lock 擁有者依序為 root、probe；結束時 trap 還原 `fs.protected_regular`）

- [ ] **Step 7: 只清除本次建立的容器與映像**

```bash
docker rm -f runner-e2e
docker rmi runner-systemd-e2e:local
if [ "$(cat "$SP/e2e/debian-preexisted")" != 1 ]; then docker rmi debian:12; fi
```

Expected: 容器與 `runner-systemd-e2e:local` 被移除；`debian:12` 只在驗證前不存在時才移除

- [ ] **Step 8: macOS 以隔離的 HOME 手動確認**

```bash
set -e
if pgrep -f "runner run" >/dev/null; then echo "another runner holds the machine lock; skipping the macOS check"; exit 0; fi
mkdir -p "$SP/e2e-mac/home"
printf 'log-level = "info"\nbogus-key = true\n' > "$SP/e2e-mac/config.toml"
go build -o "$SP/e2e-mac/runner" ./cmd/runner
HOME="$SP/e2e-mac/home" "$SP/e2e-mac/runner" run --config "$SP/e2e-mac/config.toml" </dev/null >"$SP/e2e-mac/run1.out" 2>&1 || true
grep -q "unknown config key" "$SP/e2e-mac/home/Library/Logs/runner/runner.log"
env -i HOME="$SP/e2e-mac/home" PATH=/usr/bin:/bin:/usr/sbin:/sbin XPC_SERVICE_NAME=io.github.ysya.runner \
  "$SP/e2e-mac/runner" run --config "$SP/e2e-mac/config.toml" </dev/null >"$SP/e2e-mac/run2.out" 2>&1 || true
grep -q "predates" "$SP/e2e-mac/home/Library/Logs/runner/runner.log"
rm -rf "$SP/e2e-mac"
echo macos-ok
```

Expected: 輸出 `macos-ok`（log 寫入隔離 HOME 下的 `Library/Logs/runner/runner.log`；模擬 launchd 且無版本標記時出現過舊提醒；只刪除本步驟建立的目錄）。有其他 runner 在跑時會明確略過

- [ ] **Step 9: 最終全綠**

Run: `go build ./... && go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run ./... && GOOS=linux GOARCH=amd64 go test -c -o /dev/null ./cmd/runner && GOOS=linux GOARCH=amd64 go test -c -o /dev/null ./internal/config`
Expected: 全部通過，`gofmt -l` 無輸出
