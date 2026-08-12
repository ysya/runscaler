package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
