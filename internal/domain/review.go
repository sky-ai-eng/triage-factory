package domain

import (
	"fmt"
	"strings"
)

// Review-finding severity levels. These mirror the four levels the
// pr-review prompt assigns to every finding (see
// internal/ai/prompts/pr-review.txt). They are surfaced two ways: as a
// native chip in the local pre-render diff UI, and as a shields.io badge
// prepended to the comment body when the review is posted to GitHub.
//
// The canonical form is uppercase. NormalizeSeverity accepts any case so
// third-party review skills calling the gh tooling don't have to match
// exactly; SeverityBadgeMarkdown / the frontend chip key off the
// uppercase form.
const (
	SeverityBlocker = "BLOCKER"
	SeverityMajor   = "MAJOR"
	SeverityMinor   = "MINOR"
	SeverityClean   = "CLEAN"
)

// severityBadgeColor maps each level to a shields.io color. These are
// the Tailwind 500 hex values the frontend chip palette uses
// (ReviewComment.tsx), passed to shields as path-style hex so the GitHub
// badge tracks the in-app chip hue instead of drifting to shields'
// differently-shaded named colors (their named "yellow" #dfb317 vs
// Tailwind amber-500 #f59e0b, etc.).
var severityBadgeColor = map[string]string{
	SeverityBlocker: "ef4444", // red-500
	SeverityMajor:   "f97316", // orange-500
	SeverityMinor:   "f59e0b", // amber-500
	SeverityClean:   "3b82f6", // blue-500
}

// ValidSeverities is the ordered canonical set, for help text and error
// messages. Order is most-to-least severe.
var ValidSeverities = []string{SeverityBlocker, SeverityMajor, SeverityMinor, SeverityClean}

// NormalizeSeverity upper-cases and validates a severity string. An
// empty input is valid and returns "" (no badge — backward-compatible
// with callers that omit it). A non-empty value that isn't one of the
// four levels is an error so the agent gets a clear correction instead
// of silently posting an unrecognized label.
func NormalizeSeverity(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	up := strings.ToUpper(strings.TrimSpace(s))
	if _, ok := severityBadgeColor[up]; !ok {
		return "", fmt.Errorf("invalid severity %q; valid values: %s", s, strings.Join(ValidSeverities, ", "))
	}
	return up, nil
}

// SeverityBadgeMarkdown returns the shields.io badge line to prepend to a
// review comment body at GitHub-post time. Empty severity → empty string
// (the body is posted unchanged). The badge is wrapped in nested <sub>
// tags so it renders as a small inline chip ahead of the diagnosis,
// matching the convention other review bots (Codex, Copilot) use.
//
// This is the GitHub-render path only. The local pre-render UI renders a
// native chip from the stored Severity field instead — the badge markdown
// is never written into the stored comment body.
func SeverityBadgeMarkdown(severity string) string {
	if severity == "" {
		return ""
	}
	color, ok := severityBadgeColor[severity]
	if !ok {
		return ""
	}
	return fmt.Sprintf(
		"<sub><sub>![%s](https://img.shields.io/badge/%s-%s?style=flat)</sub></sub>\n\n",
		severity, severity, color,
	)
}

// ParseSeverityBadge is the exact inverse of SeverityBadgeMarkdown: given a
// comment body that may carry a leading severity badge, it returns the parsed
// severity level (one of the Severity* constants, or "" when no badge is
// present) and the body with that badge prefix stripped.
//
// This is the read half of the GitHub-native severity round-trip (TFAC-463):
// severity is no longer a stored column — it lives only baked into the GitHub
// comment body — so the server parses it back out on read to render the native
// chip and show the clean body. Kept adjacent to SeverityBadgeMarkdown, and
// implemented by reconstructing each level's exact badge via that same
// function, so the two can't drift: any change to the badge format updates both
// directions at once.
func ParseSeverityBadge(body string) (severity, stripped string) {
	for _, sev := range ValidSeverities {
		badge := SeverityBadgeMarkdown(sev)
		if badge != "" && strings.HasPrefix(body, badge) {
			return sev, body[len(badge):]
		}
	}
	return "", body
}
