package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/ysya/runscaler/internal/layout"
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show runner's own log file",
	Long: `Show the log written by runner in every launch mode. Use 'runner service
logs' instead for service-manager events such as crash loops or OOM kills.`,
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolP("follow", "f", false, "Follow log output and reopen after rotation")
	logsCmd.Flags().IntP("lines", "n", 100, "Number of lines to show")
	cmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, _ []string) error {
	id := layout.CurrentIdentity()
	cfg, err := loadConfig(cmd)
	if err != nil {
		// A root-owned 0600 config, such as /etc/runner/config.toml found
		// during config search, is readable only with sudo.
		if !id.Root && errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("%w\n\n  run: sudo runner logs", err)
		}
		return err
	}
	decision := resolveLogFile(cfg, id)
	if !decision.Enabled {
		if decision.Warning != "" {
			return errors.New(decision.Warning)
		}
		return fmt.Errorf("runner file logging is disabled by log-file = \"\"")
	}
	path := decision.Path
	lines, _ := cmd.Flags().GetInt("lines")
	if lines < 0 {
		return fmt.Errorf("lines must be >= 0")
	}
	follow, _ := cmd.Flags().GetBool("follow")
	if err := tailFile(cmd, path, lines, follow); err != nil {
		return explainLogReadError(err, path, id)
	}
	return nil
}

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

// statSystemLog checks the system log's presence; tests replace it.
var statSystemLog = os.Stat

// explainLogReadError turns the common failures into the next command to try.
func explainLogReadError(err error, path string, id layout.Identity) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		msg := fmt.Sprintf("log file %s does not exist — runner may not have started as this user yet", path)
		if !id.Root && path != layout.SystemLogFile {
			if _, sysErr := statSystemLog(layout.SystemLogFile); sysErr == nil || errors.Is(sysErr, fs.ErrPermission) {
				msg += fmt.Sprintf("\n\n  A system service logs to %s; run: sudo runner logs", layout.SystemLogFile)
			}
		}
		return errors.New(msg)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%w\n\n  run: sudo runner logs", err)
	}
	return err
}
