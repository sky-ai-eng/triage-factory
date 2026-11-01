package domain

import (
	"regexp"
	"strings"
)

// slugAllowed matches the closed set of bytes allowed in a slug:
// ASCII lowercase letters, digits, and hyphens. Everything else
// becomes a hyphen; runs of hyphens collapse; leading/trailing
// hyphens get trimmed.
var slugAllowed = regexp.MustCompile(`[a-z0-9]+`)

// Slugify converts an arbitrary string into a URL-safe kebab-case
// ASCII slug. Empty input or input that contains no [a-z0-9]
// characters returns "" — callers substitute a default or reject.
func Slugify(in string) string {
	low := strings.ToLower(strings.TrimSpace(in))
	parts := slugAllowed.FindAllString(low, -1)
	if len(parts) == 0 {
		return ""
	}
	out := strings.Join(parts, "-")
	// Cap length at 48 chars — slugs land in URLs and we don't want
	// unbounded growth from a weirdly-long display name.
	if len(out) > 48 {
		out = strings.TrimRight(out[:48], "-")
	}
	return out
}
