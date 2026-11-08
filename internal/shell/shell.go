// Package shell quotes words for a POSIX shell. It is a leaf package shared
// by the provider's container scripts, cachestore's helper scripts and the
// CLI's copy-pasteable hints, so every quoting decision lives in one place.
package shell

import (
	"strings"
	"unicode"
)

// Quote single-quotes s unconditionally, escaping embedded single quotes.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Word quotes s only when it needs quoting, so plain paths and flags stay
// readable while anything else stays correct.
func Word(s string) string {
	plain := s != "" && strings.IndexFunc(s, func(r rune) bool {
		safe := r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/._-+=:,@%", r))
		return !safe
	}) < 0
	if plain {
		return s
	}
	return Quote(s)
}
