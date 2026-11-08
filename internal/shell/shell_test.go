package shell

import "testing"

func TestQuoteAlwaysSingleQuotes(t *testing.T) {
	for in, want := range map[string]string{
		"/plain/path": "'/plain/path'",
		"it's":        `'it'\''s'`,
		"":            "''",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestWordQuotesOnlyWhenNeeded(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/runner":  "/usr/local/bin/runner",
		"--scope":                "--scope",
		"--force=false":          "--force=false",
		"/opt/my runner/runner":  "'/opt/my runner/runner'",
		"/Users/a&b/config.toml": "'/Users/a&b/config.toml'",
		"it's":                   `'it'\''s'`,
		"café":                   "'café'",
		"":                       "''",
	} {
		if got := Word(in); got != want {
			t.Errorf("Word(%q) = %s, want %s", in, got, want)
		}
	}
}
