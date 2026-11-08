package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/ysya/runscaler/internal/shell"
)

// Every command line runner prints for an operator to copy is built here, so
// hints agree on quoting and carry sudo exactly when the target is a system
// installation. A hint names the binary's path and --scope, except where
// both are redundant: a hint without sudo calls the binary plainly "runner"
// when that is what the invoking shell's PATH finds, and a hint about the
// selected installation leaves --scope out when auto detection selects that
// installation anyway.

// shellPATH is PATH as the invoking shell set it, before ensureHomebrewPath
// extends it for runner's own tool lookups.
var shellPATH = os.Getenv("PATH")

// pathRunner resolves the `runner` the invoking shell would run, or "" when
// its PATH has none.
var pathRunner = func() string {
	for _, dir := range filepath.SplitList(shellPATH) {
		if !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, "runner")
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return ""
		}
		return resolved
	}
	return ""
}

// commandBinary is how a hint names binary: plainly "runner" when a hint run
// without sudo would find this very binary on the invoking shell's PATH, its
// path otherwise. sudo searches its own secure_path, so a sudo hint keeps the
// path.
func commandBinary(binary string, sudo bool) string {
	if sudo || !filepath.IsAbs(binary) {
		return binary
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return binary
	}
	if onPath := pathRunner(); onPath != "" && onPath == resolved {
		return "runner"
	}
	return binary
}

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
	words = append(words, shell.Word(commandBinary(c.binary, c.sudo)))
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

// targetCommand renders `runner service <action> ...` for the selected
// installation. When auto detection would select that installation anyway,
// the hint leaves --scope out.
func targetCommand(target *installationTarget, action string, args ...string) string {
	if !target.Unique {
		return serviceCommand(target.BinaryPath, target.Scope, action, args...)
	}
	words := append([]string{"service", action}, args...)
	return runnerCommand{sudo: target.Scope == scopeSystem, binary: target.BinaryPath, args: words}.String()
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
