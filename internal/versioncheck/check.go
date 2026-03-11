package versioncheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	githubRepo = "ysya/runscaler"
	apiTimeout = 5 * time.Second
)

// ReleaseInfo holds metadata about a GitHub release.
type ReleaseInfo struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// Latest queries the GitHub releases API for the latest release.
func Latest(ctx context.Context) (*ReleaseInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", githubRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to check for updates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release info: %w", err)
	}
	return &release, nil
}

// IsNewer reports whether remote is a different (presumably newer) version
// than local. For "dev" builds, always returns true.
func IsNewer(local, remote string) bool {
	local = strings.TrimPrefix(local, "v")
	remote = strings.TrimPrefix(remote, "v")
	if local == "dev" || local == "" {
		return true
	}
	return local != remote
}
