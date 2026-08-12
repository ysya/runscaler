package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultLogFileName = "runner.log"
	DefaultLogMaxSize  = int64(10 * 1024 * 1024)
)

// LogFileWriter is a concurrency-safe rotating writer shared by every logger
// in a process. Rotation keeps one backup at path + ".1".
type LogFileWriter struct {
	mu      sync.Mutex
	path    string
	maxSize int64
	file    *os.File
	size    int64
}

func OpenLogFile(path string) (*LogFileWriter, error) {
	return openLogFile(path, DefaultLogMaxSize)
}

func openLogFile(path string, maxSize int64) (*LogFileWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &LogFileWriter{path: path, maxSize: maxSize, file: f, size: info.Size()}, nil
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
	backup := w.path + ".1"
	_ = os.Remove(backup)
	if err := os.Rename(w.path, backup); err != nil && !os.IsNotExist(err) {
		w.file, _ = os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		_ = os.Rename(backup, w.path)
		w.file, _ = os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		return err
	}
	w.file = f
	w.size = 0
	return nil
}

func (w *LogFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func outputWriter(file io.Writer) io.Writer {
	if file == nil {
		return os.Stdout
	}
	return io.MultiWriter(os.Stdout, file)
}
