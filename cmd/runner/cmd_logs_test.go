package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/ysya/runscaler/internal/layout"
)

func TestTailFileLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&out)
	c.SetContext(context.Background())
	if err := tailFile(c, path, 2, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "two\nthree" {
		t.Fatalf("tail = %q", got)
	}
}

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

func TestRunLogsSuggestsSudoForUnreadableConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0000 file")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("log-level = \"info\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	t.Cleanup(viper.Reset)
	c := &cobra.Command{Use: "logs"}
	c.Flags().String("config", "", "")
	c.Flags().Int("lines", 100, "")
	c.Flags().Bool("follow", false, "")
	if err := c.Flags().Set("config", path); err != nil {
		t.Fatal(err)
	}

	err := runLogs(c, nil)
	if err == nil || !strings.Contains(err.Error(), "sudo runner logs") || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("runLogs() = %v, want a permission error with the sudo hint", err)
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

func TestExplainLogReadErrorSuggestsSudoWhenOnlyTheSystemLogExists(t *testing.T) {
	original := statSystemLog
	t.Cleanup(func() { statSystemLog = original })

	const userPath = "/home/ada/.local/state/runner/runner.log"
	notFound := &fs.PathError{Op: "open", Path: userPath, Err: fs.ErrNotExist}
	nonRoot := layout.Identity{Home: "/home/ada"}

	statSystemLog = func(string) (os.FileInfo, error) { return nil, nil }
	present := explainLogReadError(notFound, userPath, nonRoot)
	if !strings.Contains(present.Error(), userPath) || !strings.Contains(present.Error(), "sudo runner logs") {
		t.Errorf("system log present: error = %v, want user path and sudo hint", present)
	}

	statSystemLog = func(string) (os.FileInfo, error) { return nil, fs.ErrPermission }
	unreadable := explainLogReadError(notFound, userPath, nonRoot)
	if !strings.Contains(unreadable.Error(), "sudo runner logs") {
		t.Errorf("system log exists but unreadable: error = %v, want sudo hint", unreadable)
	}

	statSystemLog = func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }
	absent := explainLogReadError(notFound, userPath, nonRoot)
	if !strings.Contains(absent.Error(), userPath) || strings.Contains(absent.Error(), "sudo") {
		t.Errorf("no system log: error = %v, want user path and no sudo hint", absent)
	}

	statSystemLog = func(string) (os.FileInfo, error) { return nil, nil }
	root := explainLogReadError(notFound, userPath, layout.Identity{Root: true})
	if strings.Contains(root.Error(), "sudo") {
		t.Errorf("root identity: error = %v, want no sudo hint", root)
	}
}
