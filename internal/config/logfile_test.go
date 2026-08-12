package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogFileWriterRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.log")
	w, err := openLogFile(path, 10)
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
	w, err := OpenLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logger := NewLoggerWithWriter("info", "json", w)
	logger.Info("hello-log")
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
