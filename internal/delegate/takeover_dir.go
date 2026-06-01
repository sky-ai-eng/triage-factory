package delegate

import (
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// ResolveTakeoverDir normalizes a stored takeover directory string.
// The stored value comes from instance_config.server_takeover_dir;
// empty defaults to the state-root takeovers dir (paths.TakeoversRoot,
// i.e. ~/.triagefactory/takeovers in local mode), and a leading "~/"
// (or bare "~") is expanded against the user's home dir. Other forms
// (already-absolute, relative-with-no-tilde) are returned as-is —
// callers that need a canonical absolute path apply their own
// filepath.Abs / EvalSymlinks step (Release does this via
// canonicalizeForSafetyCheck before the prefix containment check).
// Centralized so callers (handleAgentTakeover, Release, uninstall,
// tests) don't each re-implement the home-dir math.
func ResolveTakeoverDir(stored string) (string, error) {
	if stored == "" {
		return paths.TakeoversRoot(), nil
	}
	return paths.ExpandHome(stored)
}
