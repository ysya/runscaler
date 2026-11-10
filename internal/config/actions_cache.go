package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultActionsTokenIssuer = "https://token.actions.githubusercontent.com"
	DefaultActionsJWKSURL     = "https://token.actions.githubusercontent.com/.well-known/jwks"
	DefaultResultsUpstreamURL = "https://results-receiver.actions.githubusercontent.com"
)

// ActionsCacheConfig opts the manager into a private, host-local v2 receiver.
type ActionsCacheConfig struct {
	Enabled            bool     `mapstructure:"enabled"`
	ListenAddress      string   `mapstructure:"listen-address"`
	AdvertiseURL       string   `mapstructure:"advertise-url"`
	DataDir            string   `mapstructure:"data-dir"`
	MaxSizeBytes       int64    `mapstructure:"max-size-bytes"`
	MaxIdleDays        int      `mapstructure:"max-idle-days"`
	TokenIssuer        string   `mapstructure:"token-issuer"`
	JWKSURL            string   `mapstructure:"jwks-url"`
	Audiences          []string `mapstructure:"audiences"`
	UpstreamResultsURL string   `mapstructure:"upstream-results-url"`
}

func (c ActionsCacheConfig) EffectiveTokenIssuer() string {
	if c.TokenIssuer != "" {
		return c.TokenIssuer
	}
	return DefaultActionsTokenIssuer
}

func (c ActionsCacheConfig) EffectiveJWKSURL() string {
	if c.JWKSURL != "" {
		return c.JWKSURL
	}
	if c.EffectiveTokenIssuer() == DefaultActionsTokenIssuer {
		return DefaultActionsJWKSURL
	}
	return ""
}

func (c ActionsCacheConfig) EffectiveUpstreamResultsURL() string {
	if c.UpstreamResultsURL != "" {
		return c.UpstreamResultsURL
	}
	return DefaultResultsUpstreamURL
}

func (c ActionsCacheConfig) EffectiveMaxIdleDays() int {
	if c.MaxIdleDays != 0 {
		return c.MaxIdleDays
	}
	return 30
}

func (c ActionsCacheConfig) Validate(healthPort int) error {
	if !c.Enabled {
		return nil
	}
	host, portText, err := net.SplitHostPort(c.ListenAddress)
	if err != nil {
		return errors.New("actions-cache.listen-address must include a private IP and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsPrivate() || ip.IsUnspecified() {
		return errors.New("actions-cache.listen-address must use a private IP")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("actions-cache.listen-address port must be between 1 and 65535")
	}
	if port == healthPort {
		return errors.New("actions-cache.listen-address must not use health-port")
	}
	if err := validateActionsCacheURL(c.AdvertiseURL); err != nil || c.AdvertiseURL == "" {
		return errors.New("actions-cache.advertise-url must be an absolute HTTP(S) URL with a trailing slash and no credentials")
	}
	advertise, _ := url.Parse(c.AdvertiseURL)
	if advertise.Path != "/" {
		return errors.New("actions-cache.advertise-url must use the root path")
	}
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) == "/" {
		return errors.New("actions-cache.data-dir must be an absolute private directory")
	}
	if c.MaxSizeBytes <= 0 {
		return errors.New("actions-cache.max-size-bytes must be positive")
	}
	if c.MaxIdleDays < 0 {
		return errors.New("actions-cache.max-idle-days must be positive")
	}
	issuer := c.EffectiveTokenIssuer()
	if !validOriginURL(issuer, true) {
		return errors.New("actions-cache.token-issuer must be HTTPS")
	}
	if c.EffectiveJWKSURL() == "" || !validOriginURL(c.EffectiveJWKSURL(), true) {
		return errors.New("actions-cache.jwks-url must be HTTPS, and required for a custom issuer")
	}
	if !validOriginURL(c.EffectiveUpstreamResultsURL(), true) {
		return errors.New("actions-cache.upstream-results-url must be HTTPS")
	}
	for _, audience := range c.Audiences {
		if audience == "" {
			return fmt.Errorf("actions-cache.audiences must not contain an empty value")
		}
	}
	return nil
}

func validOriginURL(value string, httpsOnly bool) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") {
		return false
	}
	if httpsOnly {
		return parsed.Scheme == "https"
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http"
}

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
