package main

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/ysya/runscaler/internal/shell"
)

// Every command line runner prints for an operator to copy is built here, so
// hints agree on quoting, name the scope with --scope, and carry sudo exactly
// when the target is a system installation.

// runnerCommand is one invocation of the runner binary.
type runnerCommand struct {
	sudo   bool
	binary string   // absolute path, or "runner" when the path is unknown
	args   []string // raw words; quoted when rendered
}

func (c runnerCommand) String() string {
	words := make([]string, 0, len(c.args)+2)
	if c.sudo {
		words = append(words, "sudo")
	}
	words = append(words, shell.Word(c.binary))
	for _, arg := range c.args {
		words = append(words, shell.Word(arg))
	}
	return strings.Join(words, " ")
}

// serviceCommand renders `runner service <action> --scope <scope> ...` for
// binary. A system scope needs root, so the command carries sudo.
func serviceCommand(binary string, scope serviceScope, action string, args ...string) string {
	words := append([]string{"service", action, "--scope", string(scope)}, args...)
	return runnerCommand{sudo: scope == scopeSystem, binary: binary, args: words}.String()
}

// serviceInstallCommand renders an install, or a --force regeneration, with
// every path explicit, so the hint reproduces exactly what runner would
// install instead of relying on auto-detection in another shell.
func serviceInstallCommand(binary string, scope serviceScope, configPath string, force, noStart bool, environmentFiles []string) string {
	var args []string
	if force {
		args = append(args, "--force")
	}
	if noStart {
		args = append(args, "--no-start")
	}
	if configPath != "" {
		args = append(args, "--config-path", configPath)
	}
	args = append(args, "--binary-path", binary)
	for _, path := range environmentFiles {
		args = append(args, "--environment-file", path)
	}
	return serviceCommand(binary, scope, "install", args...)
}

// hintBinary is the binary a hint should name: the selected installation's,
// else the running executable, else plain "runner".
func hintBinary(c *cobra.Command) string {
	if target := targetInContext(c); target != nil && target.BinaryPath != "" {
		return target.BinaryPath
	}
	return currentBinaryPath()
}
