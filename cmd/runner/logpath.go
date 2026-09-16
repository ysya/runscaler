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
