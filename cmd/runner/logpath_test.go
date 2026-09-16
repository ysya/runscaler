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
