package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Leaked-registration sweep modes (sweep-mode).
const (
	SweepModeOff    = "off"
	SweepModeLog    = "log"
	SweepModeRemove = "remove"
)

// EffectiveSweepMode is the configured sweep-mode, or DefaultSweepMode when
// unset.
func (ss *ScaleSetConfig) EffectiveSweepMode() string {
	if ss.SweepMode == "" {
		return DefaultSweepMode
	}
	return ss.SweepMode
}

// EffectiveSweepInterval is the configured sweep-interval, or
// DefaultSweepInterval when unset.
func (ss *ScaleSetConfig) EffectiveSweepInterval() time.Duration {
	if ss.SweepInterval == 0 {
		return DefaultSweepInterval
	}
	return ss.SweepInterval
}

// SweepOrganization returns the organization of an org-level registration
// URL (https://github.com/<org>). ok is false for repository and enterprise
// URLs.
func (ss *ScaleSetConfig) SweepOrganization() (org string, ok bool) {
	u, err := url.Parse(ss.RegistrationURL)
	if err != nil {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 1 || parts[0] == "" {
		return "", false
	}
	return parts[0], true
}

func (ss *ScaleSetConfig) sweepErrors() []error {
	var problems []error
	mode := ss.EffectiveSweepMode()
	switch mode {
	case SweepModeOff, SweepModeLog, SweepModeRemove:
	default:
		problems = append(problems, fmt.Errorf("sweep-mode must be %q, %q or %q", SweepModeOff, SweepModeLog, SweepModeRemove))
	}
	if ss.SweepInterval != 0 && ss.SweepInterval < MinSweepInterval {
		problems = append(problems, fmt.Errorf("sweep-interval must be at least %s (each sweep spends GitHub API quota the listener shares)", MinSweepInterval))
	}
	if mode != SweepModeOff && ss.RegistrationURL != "" {
		if _, ok := ss.SweepOrganization(); !ok {
			problems = append(problems, fmt.Errorf("sweep-mode %q requires an organization registration URL: the sweep lists runners through GitHub's organization runner-group API, which repository and enterprise scale sets do not use", mode))
		}
	}
	return problems
}

// GitHubAppPrivateKeyPEM returns the validated PEM text of the configured
// GitHub App key. Callers must never log or print it.
func (ss *ScaleSetConfig) GitHubAppPrivateKeyPEM() (string, error) {
	if ss.GitHubApp == nil {
		return "", fmt.Errorf("github-app is not configured")
	}
	return ss.GitHubApp.privateKey()
}
