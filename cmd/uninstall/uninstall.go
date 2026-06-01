// Package uninstall implements the `triagefactory uninstall` CLI
// subcommand: a one-shot wipe of all local state created by the
// running binary. Mirrors scripts/clean-slate.sh but ships inside the
// binary so users who installed via Homebrew (and therefore don't have
// the repo) still have a clean exit.
//
// What it removes:
//   - ~/.triagefactory/ in full (db, config, bare repo clones, and
//     default-location takeovers)
//   - the corresponding ~/.claude/projects/<encoded> session JSONL dirs
//     for any takeovers AND for any per-project Curator working
//     directories (enumerated BEFORE ~/.triagefactory/ is deleted, so
//     we can still resolve their absolute paths to compute the encoded
//     name Claude Code uses)
//   - all keychain entries under the "triagefactory" service
//   - the symlink left by `triagefactory install` at its default
//     destination, when present
//
// What it does NOT remove:
//   - the binary itself. With Homebrew installs, that's `brew uninstall
//     triagefactory`. We can't reliably (or safely) do it for the user
//     because the running process owns the file we'd be removing.
//
// Destructive and irreversible — defaults to interactive confirmation,
// `--yes` skips. Best-effort: each step is independent, failures are
// logged, and the command exits non-zero only when something actually
// failed.
package uninstall

import (
	"bufio"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// Handle dispatches the uninstall subcommand.
func Handle(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	dataDir := paths.StateRoot()
	// home is still needed for the ~/.claude/projects session-JSONL
	// cleanup below — that's Claude Code SDK state keyed to the real
	// HOME, not TF state, so it does not live under the (possibly
	// relocated) state root. Resolved via ExpandHome so os.UserHomeDir
	// stays confined to internal/paths.
	home, err := paths.ExpandHome("~")
	if err != nil {
		fail("resolve home dir: %v", err)
	}
	takeoversDir, takeoversDirErr := resolvedTakeoversDir(dataDir)
	linkPath := defaultInstallLink()

	plan := buildPlan(dataDir, takeoversDir, linkPath)
	if plan.empty() {
		fmt.Println("triagefactory: no on-disk local state found.")
		fmt.Println("Stored keychain credentials may still be present and can be removed.")
		fmt.Println()
		fmt.Println("This is irreversible. The binary itself stays — remove it with `brew uninstall triagefactory` (or by hand for source builds).")

		if !*yes && !confirm("Clear stored credentials? [y/N] ") {
			fmt.Println("aborted.")
			os.Exit(1)
		}

		if err := clearAllSecrets(); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: clear keychain: %v\n", err)
			fmt.Println()
			fmt.Println("triagefactory uninstall: completed with warnings (see above).")
			os.Exit(1)
		}

		fmt.Println("  cleared keychain entries")
		fmt.Println()
		fmt.Println("triagefactory uninstall: done. To remove the binary, run `brew uninstall triagefactory`.")
		return
	}

	fmt.Println("triagefactory uninstall — about to remove:")
	for _, line := range plan.summary() {
		fmt.Printf("  - %s\n", line)
	}
	fmt.Println()
	fmt.Println("This is irreversible. The binary itself stays — remove it with `brew uninstall triagefactory` (or by hand for source builds).")

	if !*yes && !confirm("Proceed? [y/N] ") {
		fmt.Println("aborted.")
		os.Exit(1)
	}

	failed := false

	if takeoversDirErr != nil {
		fmt.Fprintf(os.Stderr, "  warn: resolve server.takeover_dir: %v (falling back to %s)\n", takeoversDirErr, takeoversDir)
		failed = true
	}

	// Order: enumerate takeover/curator dirs and clear their Claude
	// project entries BEFORE removing the trees, otherwise we lose the
	// inputs needed to compute the encoded names.
	if plan.hasTakeovers {
		n, err := removeClaudeProjectsForTakeovers(plan.takeoversDir, home)
		if n > 0 {
			fmt.Printf("  removed %d Claude Code session entr%s for takeovers\n", n, plural(n, "y", "ies"))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: remove Claude Code session entries for takeovers: %v\n", err)
			failed = true
		}
		if !isSubpathOf(plan.takeoversDir, dataDir) {
			if err := os.RemoveAll(plan.takeoversDir); err != nil {
				fmt.Fprintf(os.Stderr, "  warn: remove %s: %v\n", plan.takeoversDir, err)
				failed = true
			} else {
				fmt.Printf("  removed %s\n", plan.takeoversDir)
			}
		}
	}

	// Curator's per-project working dirs at ~/.triagefactory/projects/<id>/
	// each get a corresponding ~/.claude/projects/<encoded> entry where
	// Claude Code stores the curator session JSONL. Walk and clear those
	// before RemoveAll(dataDir) takes the projects dir with it.
	if plan.hasProjects {
		n, err := removeClaudeProjectsForCurator(plan.projectsDir, home)
		if n > 0 {
			fmt.Printf("  removed %d Claude Code session entr%s for curator projects\n", n, plural(n, "y", "ies"))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: remove Claude Code session entries for curator projects: %v\n", err)
			failed = true
		}
	}

	if plan.hasDataDir {
		if err := os.RemoveAll(dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "  warn: remove %s: %v\n", dataDir, err)
			failed = true
		} else {
			fmt.Printf("  removed %s\n", dataDir)
		}
	}

	if err := clearAllSecrets(); err != nil {
		fmt.Fprintf(os.Stderr, "  warn: clear keychain: %v\n", err)
		failed = true
	} else {
		fmt.Println("  cleared keychain entries")
	}

	if plan.hasInstallLink {
		info, err := os.Lstat(linkPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: inspect %s: %v\n", linkPath, err)
			failed = true
		} else if info.Mode()&os.ModeSymlink == 0 {
			fmt.Fprintf(os.Stderr, "  warn: skip removing %s: path exists but is not a symlink\n", linkPath)
		} else {
			target, err := os.Readlink(linkPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  warn: inspect symlink target %s: %v\n", linkPath, err)
				failed = true
			} else {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(linkPath), target)
				}

				exePath, err := os.Executable()
				if err != nil {
					fmt.Fprintf(os.Stderr, "  warn: resolve current executable: %v\n", err)
					failed = true
				} else {
					resolvedTarget := target
					if p, err := filepath.EvalSymlinks(target); err == nil {
						resolvedTarget = p
					}

					resolvedExe := exePath
					if p, err := filepath.EvalSymlinks(exePath); err == nil {
						resolvedExe = p
					}

					if filepath.Clean(resolvedTarget) != filepath.Clean(resolvedExe) {
						fmt.Fprintf(os.Stderr, "  warn: skip removing %s: symlink points to %q, not the current executable %q\n", linkPath, target, exePath)
					} else if err := os.Remove(linkPath); err != nil {
						fmt.Fprintf(os.Stderr, "  warn: remove %s: %v (try: sudo rm %q)\n", linkPath, err, linkPath)
						failed = true
					} else {
						fmt.Printf("  removed install symlink %s\n", linkPath)
					}
				}
			}
		}
	}

	fmt.Println()
	if failed {
		fmt.Println("triagefactory uninstall: completed with warnings (see above).")
		os.Exit(1)
	}
	fmt.Println("triagefactory uninstall: done. To remove the binary, run `brew uninstall triagefactory`.")
}

// uninstallPlan is the set of artifacts present on disk at invocation
// time. We snapshot this before doing anything so we can show the user
// an accurate "about to remove" list without lying about things that
// were never there in the first place.
type uninstallPlan struct {
	dataDir        string
	takeoversDir   string
	projectsDir    string
	linkPath       string
	hasDataDir     bool
	hasTakeovers   bool
	hasProjects    bool
	hasInstallLink bool
}

func (p uninstallPlan) empty() bool {
	// Keychain entries aren't probed here — go-keyring's only "exists?"
	// API is a Get, which on macOS prompts the user for permission to
	// read each item. Probing here would prompt 6 times before the
	// user even said yes. The Clear() call later is no-op for missing
	// keys, so it's safe to always run.
	return !p.hasDataDir && !p.hasTakeovers && !p.hasProjects && !p.hasInstallLink
}

func (p uninstallPlan) summary() []string {
	var lines []string
	if p.hasDataDir {
		lines = append(lines, fmt.Sprintf("%s/ (database, config, repo clones)", p.dataDir))
	}
	if p.hasTakeovers {
		lines = append(lines, fmt.Sprintf("takeovers under %s", p.takeoversDir))
		lines = append(lines, "Claude Code session entries under ~/.claude/projects/ for any takeovers")
	}
	if p.hasProjects {
		lines = append(lines, "Claude Code session entries under ~/.claude/projects/ for any curator projects")
	}
	lines = append(lines, "credentials in the OS keychain (GitHub + Jira tokens)")
	if p.hasInstallLink {
		lines = append(lines, fmt.Sprintf("install symlink at %s", p.linkPath))
	}
	return lines
}

func buildPlan(dataDir, takeoversDir, linkPath string) uninstallPlan {
	projectsDir := filepath.Join(dataDir, "projects")
	p := uninstallPlan{dataDir: dataDir, takeoversDir: takeoversDir, projectsDir: projectsDir, linkPath: linkPath}

	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		p.hasDataDir = true
	}
	if p.takeoversDir != "" {
		if info, err := os.Stat(p.takeoversDir); err == nil && info.IsDir() {
			p.hasTakeovers = true
		}
	}
	if info, err := os.Stat(projectsDir); err == nil && info.IsDir() {
		p.hasProjects = true
	}
	// Lstat — we want the symlink itself, not its target. A broken
	// symlink (target removed) still counts as something to clean up.
	if linkPath != "" {
		if _, err := os.Lstat(linkPath); err == nil {
			p.hasInstallLink = true
		}
	}
	return p
}

// claudeProjectReplacer encodes an absolute path to the directory name
// Claude Code uses under ~/.claude/projects/. Every '/' and '.' becomes '-'.
// Mirrors encodeClaudeProjectDir in internal/worktree; kept local to avoid
// pulling in that package just for path encoding.
var claudeProjectReplacer = strings.NewReplacer("/", "-", ".", "-")

// removeClaudeProjectsForTakeovers walks takeover run dirs and deletes
// the matching ~/.claude/projects/<encoded> dir for each.
// Returns the number of project dirs successfully removed.
func removeClaudeProjectsForTakeovers(takeoversDir, home string) (int, error) {
	entries, err := os.ReadDir(takeoversDir)
	if err != nil {
		return 0, err
	}
	count := 0
	var joinedErr error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "run-") {
			continue
		}
		full := filepath.Join(takeoversDir, entry.Name())
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			resolved = full
		}
		encoded := claudeProjectReplacer.Replace(resolved)
		projectDir := filepath.Join(home, ".claude", "projects", encoded)
		if _, err := os.Stat(projectDir); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			joinedErr = errors.Join(joinedErr, fmt.Errorf("inspect %s: %w", projectDir, err))
			continue
		}
		if err := os.RemoveAll(projectDir); err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("remove %s: %w", projectDir, err))
			continue
		}
		count++
	}
	return count, joinedErr
}

// removeClaudeProjectsForCurator walks ~/.triagefactory/projects/<id>/
// dirs (the Curator's per-project working directories) and deletes the
// matching ~/.claude/projects/<encoded> dir for each. Symmetric to
// removeClaudeProjectsForTakeovers — same encoding rule, different
// source of cwds. Without this, curator session JSONLs orphan in
// ~/.claude/projects/ after uninstall.
func removeClaudeProjectsForCurator(projectsDir, home string) (int, error) {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return 0, err
	}
	count := 0
	var joinedErr error
	for _, entry := range entries {
		// Every immediate subdir of projects/ is a project ID — there's
		// no naming filter the way takeovers/ has run-* dirs alongside
		// other state. Skip non-dirs defensively.
		if !entry.IsDir() {
			continue
		}
		full := filepath.Join(projectsDir, entry.Name())
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			resolved = full
		}
		encoded := claudeProjectReplacer.Replace(resolved)
		projectDir := filepath.Join(home, ".claude", "projects", encoded)
		if _, err := os.Stat(projectDir); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			joinedErr = errors.Join(joinedErr, fmt.Errorf("inspect %s: %w", projectDir, err))
			continue
		}
		if err := os.RemoveAll(projectDir); err != nil {
			joinedErr = errors.Join(joinedErr, fmt.Errorf("remove %s: %w", projectDir, err))
			continue
		}
		count++
	}
	return count, joinedErr
}

func resolvedTakeoversDir(dataDir string) (string, error) {
	fallback := filepath.Join(dataDir, "takeovers")

	// instance_config.server_takeover_dir is the persisted override;
	// read it directly here rather than via a wrapper package. Probe
	// for the DB file first so a fresh machine (no DB) doesn't
	// materialize state we're about to delete.
	dbPath := filepath.Join(dataDir, "triagefactory.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return fallback, nil
		}
		return fallback, err
	}
	conn, err := db.OpenAt(dbPath)
	if err != nil {
		return fallback, err
	}
	defer conn.Close()
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		return fallback, err
	}
	var stored string
	if err := conn.QueryRow(`SELECT server_takeover_dir FROM instance_config WHERE id = 1`).Scan(&stored); err != nil {
		// No row → use fallback; any other error propagates.
		if errors.Is(err, sql.ErrNoRows) {
			return fallback, nil
		}
		return fallback, err
	}
	dir, err := delegate.ResolveTakeoverDir(stored)
	if err != nil {
		return fallback, err
	}
	if dir == "" {
		return fallback, nil
	}
	return filepath.Clean(dir), nil
}

// isSubpathOf reports whether path is lexically contained by parent.
// Used to avoid double-deleting takeover dirs already covered by
// removing ~/.triagefactory itself.
func isSubpathOf(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// defaultInstallLink mirrors the destination logic in cmd/install. We
// don't try to discover non-default locations: if the user passed
// --dest somewhere weird at install time, they know where it went and
// can remove it themselves. False positives would be worse than false
// negatives here.
func defaultInstallLink() string {
	if runtime.GOOS == "darwin" {
		return "/usr/local/bin/triagefactory"
	}
	// Binary symlink location (~/.local/bin), not TF state — resolved via
	// ExpandHome so os.UserHomeDir stays confined to internal/paths.
	link, err := paths.ExpandHome("~/.local/bin/triagefactory")
	if err != nil {
		return ""
	}
	return link
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func plural(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// clearAllSecrets removes every credential key the SecretStore
// manages plus any legacy keys still swept on Clear. Uninstall is
// single-process, local-only, with no DB context, so it bypasses the
// SecretStore seam and calls the low-level keychain helpers directly
// — but the key list comes from integrations.AllKeys() so this stays
// in sync as new keys land.
func clearAllSecrets() error {
	for _, k := range integrations.AllKeys() {
		if err := auth.DeleteSecret(k); err != nil {
			return err
		}
	}
	return nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory uninstall: "+format+"\n", args...)
	os.Exit(1)
}
