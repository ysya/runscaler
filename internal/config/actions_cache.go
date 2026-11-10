package config

import (
	"errors"
	"net/url"
	"strings"
)

// validateActionsCacheURL never includes the input in an error: malformed URLs
// can contain credentials even when parsing fails.
func validateActionsCacheURL(value string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || strings.Contains(value, "#") || !strings.HasSuffix(value, "/") {
		return errors.New("docker.actions-cache-url must be an absolute HTTP(S) URL with a host and trailing slash, without credentials, query or fragment")
	}
	return nil
}
