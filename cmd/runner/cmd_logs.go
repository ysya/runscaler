package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	path, enabled := resolveLogFilePath(cfg)
	if !enabled {
		return fmt.Errorf("runner file logging is disabled by log-file = \"\"")
	}
	lines, _ := cmd.Flags().GetInt("lines")
	if lines < 0 {
		return fmt.Errorf("lines must be >= 0")
	}
	follow, _ := cmd.Flags().GetBool("follow")
	if err := tailFile(cmd, path, lines, follow); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("log file %s does not exist — runner may not have started with this config yet", path)
		}
		return err
	}
	return nil
}

func tailFile(cmd *cobra.Command, path string, lines int, follow bool) error {
	data, err := os.ReadFile(path)
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
	var identity os.FileInfo
	if identity, err = os.Stat(path); err != nil {
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
		if !os.SameFile(identity, info) || info.Size() < offset {
			offset = 0
			identity = info
		}
		if info.Size() == offset {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			fmt.Fprintln(cmd.OutOrStdout(), scanner.Text())
		}
		offset, _ = f.Seek(0, io.SeekCurrent)
		scanErr := scanner.Err()
		f.Close()
		if scanErr != nil {
			return scanErr
		}
	}
}
