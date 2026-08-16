package server

// This file is the API-contract ratchet: a source scan that fails on the
// error-handling patterns the backend cleanup is eliminating, everywhere
// except an explicit, shrinking allowlist. The mechanics only tighten:
//
//   - A new occurrence in a file with no entry fails immediately. The fix is
//     to use the httpx kernel (WriteErrors / DecodeJSONStrict), not to add an
//     entry — reviewers treat a new entry as a red flag.
//   - A stale entry — one whose pattern no longer occurs in its file — fails
//     the test, so entries can only be deleted as files convert, never
//     accumulate as dead grandfathering.
//   - The permanent section documents surfaces that are deliberately outside
//     the JSON contract (protocol-constrained response shapes); each entry
//     says why. It is subject to the same staleness check.
//
// Scope: production .go files under internal/server/... and ee/... .
// _test.go files are skipped (fake upstream servers legitimately speak other
// shapes), and internal/server/httpx is skipped wholesale — it IS the kernel
// these patterns are supposed to route through.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ratchetPatterns are the banned constructs. Matching runs on comment-stripped
// source so prose mentioning a pattern doesn't count, but string literals do
// (the error-map pattern includes one).
var ratchetPatterns = map[string]*regexp.Regexp{
	// Plain-text error writers on JSON routes.
	"http.Error":    regexp.MustCompile(`http\.Error\(`),
	"http.NotFound": regexp.MustCompile(`http\.NotFound\(`),
	// Raw request-body decoding outside the kernel (no strictness, no cap).
	"request-decode": regexp.MustCompile(`json\.NewDecoder\((?:r|req)\.Body\)`),
	// Ad-hoc single-string error bodies, single-line or multi-line.
	"error-map": regexp.MustCompile(`map\[string\](?:string|any|interface\{\})\{\s*"error"`),
}

type ratchetEntry struct {
	file    string // slash path relative to the repo root
	pattern string // key into ratchetPatterns
}

// ratchetAllowlist is the SHRINKING section: files not yet converted to the
// httpx envelope/strict decoding. Delete an entry when you convert its file —
// the staleness check forces it. Never add one.
var ratchetAllowlist = []ratchetEntry{
	// http.Error — plain-text bodies on JSON surfaces.
	{"internal/server/auth_handlers.go", "http.Error"},
	{"internal/server/avatars_handler.go", "http.Error"},
	{"internal/server/github_app_register.go", "http.Error"},
	{"internal/server/middleware.go", "http.Error"},
	{"internal/server/ratelimit.go", "http.Error"},

	// http.NotFound — Go's text-body 404 on JSON surfaces.
	{"internal/server/auth_handlers.go", "http.NotFound"},
	{"internal/server/authz/authz.go", "http.NotFound"},
	{"internal/server/avatars_handler.go", "http.NotFound"},
	{"internal/server/github_app_register.go", "http.NotFound"},
	{"internal/server/invites_handler.go", "http.NotFound"},
	{"internal/server/marketplace_handler.go", "http.NotFound"},
	{"internal/server/middleware.go", "http.NotFound"},
	{"internal/server/org_members_handler.go", "http.NotFound"},
	{"internal/server/orgs_handler.go", "http.NotFound"},
	{"internal/server/team_archive_handler.go", "http.NotFound"},
	{"internal/server/team_members_handler.go", "http.NotFound"},
	{"internal/server/teams_handler.go", "http.NotFound"},
	{"internal/server/usage_access_log.go", "http.NotFound"},
	{"internal/server/usage_activity_handler.go", "http.NotFound"},
	{"internal/server/usage_handler.go", "http.NotFound"},
	{"ee/slack/channels_handler.go", "http.NotFound"},
	{"ee/slack/workspaces.go", "http.NotFound"},
	{"ee/sso/connection.go", "http.NotFound"},
	{"ee/sso/domains.go", "http.NotFound"},

	// request-decode — raw json.NewDecoder on a request body.
	{"ee/fleet/handlers.go", "request-decode"},

	// error-map — ad-hoc {"error": "..."} bodies.
	{"internal/server/authz/authz.go", "error-map"},
	{"internal/server/backfill.go", "error-map"},
	{"internal/server/bedrock_connect.go", "error-map"},
	{"internal/server/bedrock_role.go", "error-map"},
	{"internal/server/blueprints_handler.go", "error-map"},
	{"internal/server/credentials.go", "error-map"},
	{"internal/server/event_handlers_handler.go", "error-map"},
	{"internal/server/fleet_placement.go", "error-map"},
	{"internal/server/fleet_queue.go", "error-map"},
	{"internal/server/github_access.go", "error-map"},
	{"internal/server/github_app_handlers.go", "error-map"},
	{"internal/server/github_app_import.go", "error-map"},
	{"internal/server/github_app_register.go", "error-map"},
	{"internal/server/github_connect.go", "error-map"},
	{"internal/server/github_webhook_health.go", "error-map"},
	{"internal/server/invites_handler.go", "error-map"},
	{"internal/server/jira_app_handlers.go", "error-map"},
	{"internal/server/jira_connect.go", "error-map"},
	{"internal/server/marketplace_handler.go", "error-map"},
	{"internal/server/org_credentials.go", "error-map"},
	{"internal/server/org_members_handler.go", "error-map"},
	{"internal/server/orgs_handler.go", "error-map"},
	{"internal/server/project_entities.go", "error-map"},
	{"internal/server/projects.go", "error-map"},
	{"internal/server/projects_knowledge_store.go", "error-map"},
	{"internal/server/prompts_handler.go", "error-map"},
	{"internal/server/repos_handler.go", "error-map"},
	{"internal/server/settings.go", "error-map"},
	{"internal/server/settings_handlers.go", "error-map"},
	{"internal/server/skills_handler.go", "error-map"},
	{"internal/server/team_archive_handler.go", "error-map"},
	{"internal/server/team_members_handler.go", "error-map"},
	{"internal/server/teams_handler.go", "error-map"},
	{"internal/server/usage_activity_handler.go", "error-map"},
	{"internal/server/usage_ops_handler.go", "error-map"},
	{"ee/slack/workspaces.go", "error-map"},
	{"ee/sso/breakglass.go", "error-map"},
	{"ee/sso/connection.go", "error-map"},
	{"ee/sso/domains.go", "error-map"},
}

// ratchetPermanent documents surfaces deliberately outside the JSON error
// contract. These are not debt; each entry says why the shape is dictated by
// something other than this API. Still staleness-checked so a deleted surface
// takes its entry with it.
var ratchetPermanent = []ratchetEntry{
	// Webhook receivers answer the provider's expected shapes, not ours.
	{"internal/server/github_webhooks.go", "http.Error"},
	{"internal/server/github_webhooks.go", "http.NotFound"},
	{"ee/slack/webhook.go", "http.Error"},
	// GoTrue admin-proxy deny: deliberately indistinguishable from an
	// unknown path, so it must be Go's stock text 404.
	{"internal/server/auth_proxy.go", "http.NotFound"},
	// OAuth connect/callback GETs are browser-navigation surfaces that error
	// via redirects/plain text, never consumed as JSON by the SPA.
	{"internal/server/github_connect.go", "http.NotFound"},
	{"internal/server/jira_connect.go", "http.NotFound"},
	// SSO login start/callback: same browser-redirect surface as OAuth.
	{"ee/sso/login.go", "http.Error"},
	{"ee/sso/login.go", "http.NotFound"},
	// The SPA fallback serves static assets; its errors are not API errors.
	{"internal/server/server.go", "http.Error"},
	{"internal/server/server.go", "http.NotFound"},
}

// TestRatchet_NoNewBannedPatterns is the enforcement described at the top of
// this file.
func TestRatchet_NoNewBannedPatterns(t *testing.T) {
	repoRoot := "../.."
	roots := []string{"internal/server", "ee"}

	allowed := map[ratchetEntry]bool{}
	for _, e := range append(append([]ratchetEntry{}, ratchetAllowlist...), ratchetPermanent...) {
		if allowed[e] {
			t.Fatalf("duplicate ratchet entry: %s / %s", e.file, e.pattern)
		}
		allowed[e] = true
	}

	hits := map[ratchetEntry][]int{}
	for _, root := range roots {
		rootPath := filepath.Join(repoRoot, root)
		if _, err := os.Stat(rootPath); err != nil {
			t.Fatalf("scan root %s missing: %v", root, err)
		}
		err := filepath.WalkDir(rootPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The kernel is where these patterns are supposed to live.
				if filepath.Base(path) == "httpx" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := stripGoComments(string(raw))
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			for name, re := range ratchetPatterns {
				for _, loc := range re.FindAllStringIndex(src, -1) {
					line := 1 + strings.Count(src[:loc[0]], "\n")
					e := ratchetEntry{file: rel, pattern: name}
					hits[e] = append(hits[e], line)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	var violations []string
	for e, lines := range hits {
		if !allowed[e] {
			violations = append(violations, fmt.Sprintf(
				"%s: banned pattern %q at line(s) %v — route errors through httpx (WriteErrors/DecodeJSONStrict) instead of adding an allowlist entry",
				e.file, e.pattern, lines))
		}
	}
	for _, e := range ratchetAllowlist {
		if len(hits[e]) == 0 {
			violations = append(violations, fmt.Sprintf(
				"stale allowlist entry: %s no longer contains %q — delete the entry so the ratchet tightens",
				e.file, e.pattern))
		}
	}
	for _, e := range ratchetPermanent {
		if len(hits[e]) == 0 {
			violations = append(violations, fmt.Sprintf(
				"stale permanent entry: %s no longer contains %q — remove it (the surface it excused is gone)",
				e.file, e.pattern))
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

// stripGoComments blanks line and block comments (preserving newlines so
// reported line numbers stay true) while leaving string and rune literals
// intact — the error-map pattern matches a string literal, and a pattern
// mentioned in prose must not count as a hit.
func stripGoComments(src string) string {
	out := []byte(src)
	const (
		code = iota
		lineComment
		blockComment
		dquote
		backquote
		rune_
	)
	state := code
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				state = lineComment
				out[i] = ' '
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				state = blockComment
				out[i] = ' '
			case c == '"':
				state = dquote
			case c == '`':
				state = backquote
			case c == '\'':
				state = rune_
			}
		case lineComment:
			if c == '\n' {
				state = code
			} else {
				out[i] = ' '
			}
		case blockComment:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = code
			} else if c != '\n' {
				out[i] = ' '
			}
		case dquote:
			switch c {
			case '\\':
				i++
			case '"':
				state = code
			}
		case backquote:
			if c == '`' {
				state = code
			}
		case rune_:
			switch c {
			case '\\':
				i++
			case '\'':
				state = code
			}
		}
	}
	return string(out)
}
