// Package uninstall implements the `triagefactory uninstall` CLI
// subcommand: a one-shot wipe of all local state created by the
// running binary. Mirrors scripts/clean-slate.sh but ships inside the
// binary so users who installed via Homebrew (and therefore don't have
// the repo) still have a clean exit.
//
// What it removes:
//   - ~/.triagefactory/ in full (db, config, bare repo clones, workspace snapshot blobs)
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
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
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

	dataDir, err := paths.StateRootErr()
	if err != nil {
		fail("resolve state dir: %v", err)
	}
	// Resolve every subpath through internal/paths here, at the single
	// boundary, then thread them into the (pure) plan helpers. Safe to use
	// the error-free resolvers now that the StateRootErr pre-flight above
	// succeeded.
	linkPath := defaultInstallLink()

	plan := buildPlan(dataDir, linkPath)
	if plan.empty() {
		fmt.Println("triagefactory: no on-disk local state found.")
		fmt.Println("Stored keychain credentials may still be present and can be removed.")
		fmt.Println()
		fmt.Println("This is irreversible. The binary itself stays — remove it with `brew uninstall triagefactory` (or by hand for source builds).")

		if !*yes && !confirm("Clear stored credentials? [y/N] ") {
			fmt.Println("aborted.")
			os.Exit(1)
		}

		// No data dir ⇒ no DB ⇒ no GitHub App ids to enumerate; the static
		// AllLocalSweepKeys set is the whole sweep here.
		if err := clearAllSecrets(nil); err != nil {
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

	// Enumerate the per-GitHub-App keychain keys BEFORE the data-dir wipe
	// removes the DB the App ids live in (org_github_apps). Best-effort: a read
	// failure just means those keys (if any) go unswept — warn and continue. No
	// DB / no Apps yields an empty list, the common case.
	var appKeys []string
	if plan.hasDataDir {
		var err error
		appKeys, err = gitHubAppKeychainKeys(paths.DBPath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warn: enumerate GitHub App keychain keys: %v\n", err)
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

	if err := clearAllSecrets(appKeys); err != nil {
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
	linkPath       string
	hasDataDir     bool
	hasInstallLink bool
}

func (p uninstallPlan) empty() bool {
	// Keychain entries aren't probed here — go-keyring's only "exists?"
	// API is a Get, which on macOS prompts the user for permission to
	// read each item. Probing here would prompt 6 times before the
	// user even said yes. The Clear() call later is no-op for missing
	// keys, so it's safe to always run.
	return !p.hasDataDir && !p.hasInstallLink
}

func (p uninstallPlan) summary() []string {
	var lines []string
	if p.hasDataDir {
		lines = append(lines, fmt.Sprintf("%s/ (database, config, bare repo clones, workspace snapshot blobs)", p.dataDir))
	}
	lines = append(lines, "stored credentials (GitHub + Jira tokens, Anthropic API key, GitHub App keys) — OS keychain on desktop, or the encrypted secrets file (removed with the data dir above) on headless")
	if p.hasInstallLink {
		lines = append(lines, fmt.Sprintf("install symlink at %s", p.linkPath))
	}
	return lines
}

func buildPlan(dataDir, linkPath string) uninstallPlan {
	p := uninstallPlan{dataDir: dataDir, linkPath: linkPath}

	if info, err := os.Stat(dataDir); err == nil && info.IsDir() {
		p.hasDataDir = true
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

// clearAllSecrets removes every static keychain key a full local wipe touches
// (integrations.AllLocalSweepKeys — the integration credentials plus the
// org-level Anthropic / Atlassian-OAuth secrets) together with the dynamic
// per-GitHub-App keys the caller enumerated from the DB. Uninstall is
// single-process, local-only, with no DB context for the sweep itself, so it
// bypasses the SecretStore seam and calls the low-level keychain helpers
// directly — the static list comes from integrations so it stays in sync as new
// keys land. appKeys is nil when there's no DB / no configured Apps.
func clearAllSecrets(appKeys []string) error {
	// Sweep the OS keychain directly, independent of the runtime backend
	// selection: a box may still hold keychain rows from an earlier
	// keychain-backed run even if TF_SECRETS_BACKEND=file is set now (and the
	// backend-routed auth.DeleteSecret wouldn't touch the keychain in that
	// case). SweepKeychain is a no-op when the keychain is unreachable. The
	// headless file backend's bag lives under the state root and is already
	// removed by the data-dir RemoveAll above, so it needs no sweep here (and
	// uninstall never needs TF_SECRET_ENCRYPTION_KEY).
	return auth.SweepKeychain(append(integrations.AllLocalSweepKeys(), appKeys...))
}

// gitHubAppKeychainKeys enumerates the dynamic per-App keychain keys to sweep on
// uninstall. Each registered GitHub App custodies three secrets under
// github_app_<id>_{pem,client_secret,webhook_secret}; the App ids live in the
// org_github_apps table, which is still on disk here (we run before the data-dir
// wipe). Returns nil when there's no DB yet or no App is configured. Best-effort:
// any open/query failure is returned so Handle can warn, but it never blocks the
// rest of the uninstall — at worst a rare App key lingers, which the user can
// remove by hand.
//
// The SELECT is raw rather than a GitHubAppsStore call for two reasons that
// both point the same way. Uninstall opens the SQLite file by path and owns
// that handle for one query — it builds no db.Stores, and standing one up
// (both pools, the dialect split) to read one column would be more machinery
// than the teardown warrants. And the read is org-blind on purpose: it wants
// every App id in the file so no keychain entry survives the wipe, whereas the
// store's reads are all org-scoped (GetForOrg / GetForOrgSystem) and there is
// no cross-org list method — nor should there be one minted for a path that
// runs after the user has already asked for everything to be deleted.
func gitHubAppKeychainKeys(dbPath string) ([]string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no DB — nothing to enumerate
		}
		return nil, fmt.Errorf("stat %s: %w", dbPath, err)
	}

	conn, err := db.OpenAt(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer conn.Close()

	rows, err := conn.QueryContext(context.Background(), `SELECT app_id FROM org_github_apps`)
	if err != nil {
		return nil, fmt.Errorf("query org_github_apps: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var appID string
		if err := rows.Scan(&appID); err != nil {
			return nil, fmt.Errorf("scan app_id: %w", err)
		}
		if appID == "" {
			continue
		}
		keys = append(keys, integrations.GitHubAppKeysFor(appID).All()...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate org_github_apps: %w", err)
	}
	return keys, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "triagefactory uninstall: "+format+"\n", args...)
	os.Exit(1)
}
