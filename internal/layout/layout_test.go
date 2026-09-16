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
