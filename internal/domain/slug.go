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

// MaxSlugLen is the longest slug Slugify returns. Slugs land in URLs, so a
// weirdly long display name must not grow one without bound.
const MaxSlugLen = 48

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
	if len(out) > MaxSlugLen {
		out = strings.TrimRight(out[:MaxSlugLen], "-")
	}
	return out
}
