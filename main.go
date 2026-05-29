package main

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/auth/verify"
	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/poller"
	"github.com/sky-ai-eng/triage-factory/internal/projectclassify"
	"github.com/sky-ai-eng/triage-factory/internal/repoprofile"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/sessions"
	"github.com/sky-ai-eng/triage-factory/internal/skills"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sky-ai-eng/triage-factory/cmd/exec"
	"github.com/sky-ai-eng/triage-factory/cmd/install"
	"github.com/sky-ai-eng/triage-factory/cmd/jwkinit"
	"github.com/sky-ai-eng/triage-factory/cmd/migrate"
	"github.com/sky-ai-eng/triage-factory/cmd/resume"
	"github.com/sky-ai-eng/triage-factory/cmd/uninstall"
)

const (
	defaultPort = 3000
	// defaultHost binds to loopback only. Triage Factory is a local-first
	// tool that holds keychain-backed credentials and an unauthenticated
	// HTTP API; exposing it on all interfaces by default would let anyone
	// on the same network drive delegated runs. Override with --host if
	// you genuinely want LAN access.
	defaultHost = "127.0.0.1"
)

// Version is the binary's release tag, set by the linker at build time
// (`-ldflags "-X main.Version=v0.1.0"`). Local builds without that flag
// see the literal "dev" so anything in the wild claiming to be "dev" is
// known to be unreleased.
var Version = "dev"

// pluralize picks the singular or plural form of a noun based on count.
// Used for toast copy where "1 entity tracked" vs "5 entities tracked"
// reads nicer than a naive "(s)" suffix.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// validateHTTPURL parses raw as a URL and rejects anything that
// isn't an absolute http(s) URL with a host. Used by multi-mode boot
// for TF_PUBLIC_URL — that value flows into SetAuthDeps where it
// drives the OAuth redirect base and the Secure-cookie flag. An
// empty or scheme-less value would either crash on the redirect or
// silently disable Secure on session cookies (HasPrefix("https://")
// returns false on "" too). Failing at boot makes the misconfig
// loud.
func validateHTTPURL(name, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: parse %q: %w", name, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: scheme must be http or https, got %q", name, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: missing host in %q", name, raw)
	}
	return nil
}

// applyPGPoolDefaults sets connection-pool ceilings on a Postgres
// *sql.DB. database/sql's default MaxOpenConns is unlimited, which can
// exhaust Postgres' max_connections (default 100) under load — and
// multi-mode opens two pools (admin + app) against the same server, so
// the budget per pool needs to leave room for the other.
//
// The numbers below are conservative defaults that fit comfortably
// within a default supabase/postgres install (max_connections=100,
// with ~30 reserved for the image's own roles). Operators tuning a
// production deployment should raise these along with Postgres'
// max_connections; until that knob is wired, fixed defaults are
// safer than leaving the pools uncapped.
func applyPGPoolDefaults(db *sql.DB) {
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)
	db.SetConnMaxLifetime(5 * time.Minute)
}

// resolveAIModelForOrg looks up the model the org's default team uses for
// delegation, clamped by the org's max-tier cap (domain.EffectiveModel).
// Falls back to domain.DefaultTeamSettings().DefaultModel on any error so a
// transient DB hiccup doesn't silently clear the spawner+curator
// credentials. The store's GetSettingsSystem already returns
// DefaultTeamSettings() on missing rows; the explicit fallback here covers
// the other failure modes (team-lookup error, settings-read error, no
// default team at all). A failed org-settings read just means no cap is
// applied — the team default stands.
func resolveAIModelForOrg(ctx context.Context, stores db.Stores, orgID string) string {
	fallback := domain.DefaultTeamSettings().DefaultModel
	teamID, err := stores.Teams.GetDefaultForOrgSystem(ctx, orgID)
	if err != nil || teamID == "" {
		if err != nil {
			log.Printf("[main] resolve default team for org %s: %v (using default model %q)", orgID, err, fallback)
		}
		return fallback
	}
	teamSet, err := stores.Teams.GetSettingsSystem(ctx, teamID)
	if err != nil {
		log.Printf("[main] read team settings %s: %v (using default model %q)", teamID, err, fallback)
		return fallback
	}

	var maxTier string
	if orgSet, err := stores.Orgs.GetSettingsSystem(ctx, orgID); err != nil {
		log.Printf("[main] read org settings %s: %v (applying no model cap)", orgID, err)
	} else {
		maxTier = orgSet.MaxLLMModelTier
	}

	model, _ := domain.EffectiveModel(teamSet.DefaultModel, maxTier)
	return model
}

// bootstrapBareClones reads the configured repos from the DB and asks
// the worktree package to ensure each one is materialized on disk
// as a bare clone with the right origin URL.
//
// Called after profiling completes — profiling is what populates
// repo_profiles.clone_url, and BootstrapTargets without a CloneURL
// are skipped. Profiles never become non-empty without a successful
// profiling pass, so this ordering is intentional.
//
// Database read errors are logged and the bootstrap is skipped: a
// transient DB issue shouldn't crash the main path, and the lazy
// clone inside CreateForPR / CreateForBranch will recover the
// affected delegations on next run.
func bootstrapBareClones(database *sql.DB, repos db.RepoStore) {
	profiles, err := repos.ListSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		log.Printf("[worktree] bootstrap: load profiles: %v", err)
		return
	}
	targets := make([]worktree.BootstrapTarget, 0, len(profiles))
	for _, p := range profiles {
		targets = append(targets, worktree.BootstrapTarget{
			Owner:    p.Owner,
			Repo:     p.Repo,
			CloneURL: p.CloneURL,
		})
	}
	worktree.BootstrapBareClones(context.Background(), targets)
}

// bootstrapLocalGitHubIdentity populates users.github_username on the
// local synthetic user row by deriving the login from the configured
// PAT+URL. Runs at startup before seedDefaultPrompts so the SQLite
// Seed substitution sees the populated value when it wires
// author_in/reviewer_in/commenter_in allowlists into shipped event
// handler predicates.
//
// No-op when (a) the column already has a value, (b) credentials are
// absent (Settings UI capture is the alternate write path), or
// (c) ValidateGitHub fails (PAT invalid / GitHub down — the user
// can recapture via Settings, or the next boot retries).
func bootstrapLocalGitHubIdentity(users db.UsersStore, secrets db.SecretStore) error {
	if runmode.Current() != runmode.ModeLocal {
		return nil
	}
	ctx := context.Background()

	creds, _ := integrations.Load(ctx, secrets, runmode.LocalDefaultOrgID) // secret-store errors are non-fatal — degrade to no-op
	if creds.GitHubPAT == "" || creds.GitHubURL == "" {
		return nil
	}
	existing, err := users.GetGitHubUsername(ctx, runmode.LocalDefaultUserID)
	if err != nil {
		return fmt.Errorf("read users.github_username: %w", err)
	}
	if existing != "" {
		return nil
	}
	ghUser, err := auth.ValidateGitHub(creds.GitHubURL, creds.GitHubPAT)
	if err != nil {
		log.Printf("[bootstrap] derive users.github_username from PAT: %v (continuing — Settings will capture next save)", err)
		return nil
	}
	if err := users.SetGitHubUsername(ctx, runmode.LocalDefaultUserID, ghUser.Login); err != nil {
		return fmt.Errorf("persist users.github_username: %w", err)
	}
	log.Printf("[bootstrap] users.github_username: derived %q from credentials", ghUser.Login)
	return nil
}

// bootstrapLocalJiraIdentity is the Jira analog of
// bootstrapLocalGitHubIdentity. Populates users.jira_account_id and
// users.jira_display_name on the local synthetic user row by
// deriving them from the configured Jira PAT+URL. Both fields come
// from the same /rest/api/2/myself response, so the capture is one
// round-trip per boot.
//
// Runs at startup before seedDefaultPrompts so the SQLite Seed
// substitution can fill `assignee_in: []` placeholders on shipped
// jira-assigned / jira-became-atomic handler predicates with the
// local user's account ID.
//
// No-op when (a) the row already has both columns populated,
// (b) credentials are absent, or (c) ValidateJira fails. The Settings
// handler covers the alternate write path on Jira reconnect.
func bootstrapLocalJiraIdentity(users db.UsersStore, secrets db.SecretStore) error {
	if runmode.Current() != runmode.ModeLocal {
		return nil
	}
	ctx := context.Background()

	creds, _ := integrations.Load(ctx, secrets, runmode.LocalDefaultOrgID)
	if creds.JiraPAT == "" || creds.JiraURL == "" {
		return nil
	}
	existingID, existingName, err := users.GetJiraIdentity(ctx, runmode.LocalDefaultUserID)
	if err != nil {
		return fmt.Errorf("read users.jira_identity: %w", err)
	}
	if existingID != "" && existingName != "" {
		return nil
	}
	jiraUser, err := auth.ValidateJira(creds.JiraURL, creds.JiraPAT)
	if err != nil {
		log.Printf("[bootstrap] derive users.jira_identity from PAT: %v (continuing — Settings will capture next save)", err)
		return nil
	}
	accountID := jiraUser.StableID()
	if err := users.SetJiraIdentity(ctx, runmode.LocalDefaultUserID, accountID, jiraUser.DisplayName); err != nil {
		return fmt.Errorf("persist users.jira_identity: %w", err)
	}
	log.Printf("[bootstrap] users.jira_identity: derived account=%q name=%q from credentials", accountID, jiraUser.DisplayName)
	return nil
}

// printTopLevelHelp routes the two audiences (delegated Claude Code
// agents vs. human users) to the right surface. Agents almost always
// reach this through autocomplete / accidental invocation when they
// were trying to run a scoped subcommand, so the first thing they
// should see is the `exec` pointer; humans typically want the server
// flags and the takeover-resume shortcuts. Keep it short — anything
// longer goes in docs/usage.md, which we link to.
func printTopLevelHelp() {
	fmt.Println(`triagefactory — local-first AI triage for engineering backlogs.

Run with no arguments to start the server (port 3000, opens browser).

USER COMMANDS
  triagefactory                            start the server
  triagefactory --port N                   start on a custom port
  triagefactory --host <addr>              bind address (default 127.0.0.1;
                                           use 0.0.0.0 for LAN access)
  triagefactory --no-browser               start without opening a browser
  triagefactory --version                  print the binary's version
  triagefactory install [--dest <path>]    symlink the binary onto PATH
  triagefactory uninstall [--yes]          wipe local state (db, config,
                                           keychain, takeovers); leaves
                                           the binary itself in place
  triagefactory resume [<short-id>]        resume a taken-over session
                                           (auto-resumes when there's only
                                           one; picker otherwise)
  triagefactory migrate up                 bring the schema to head
  triagefactory migrate status             list applied + pending migrations

AGENT COMMANDS
  Used by delegated Claude Code agents inside their worktree, not
  meant for direct invocation by humans.

  triagefactory exec <subcommand> ...      scoped GitHub / Jira ops
                                           (run "triagefactory exec --help"
                                           for the full list)
  triagefactory status <run-id>            check a delegated run's status

For configuration, polling, and feature details, see docs/usage.md.`)
}

func main() {
	// Initialize the runtime mode flag (TF_MODE env, default local)
	// as the first thing the binary does — every dispatched
	// subcommand below runs after this so the package-level mode is
	// set before any subsystem touches a path or opens a DB. SKY-248
	// (D4a) only ships the mode flag; D4b adds the path resolvers
	// that consume it (under a separate internal/paths package).
	if err := runmode.InitFromEnv(); err != nil {
		log.Fatalf("runmode: %v", err)
	}

	// Dual-mode dispatch:
	//   exec/status — CLI-only, used by Claude Code agent.
	//   resume      — user-facing, hands the terminal back into a
	//                 previously taken-over Claude Code session.
	//   install     — user-facing, symlinks the binary onto PATH so
	//                 `triagefactory resume` works without a full path.
	//   uninstall   — user-facing, wipes everything install + the server
	//                 leave behind on the host (db, config, keychain,
	//                 takeover dirs). Doesn't remove the binary itself.
	//   -h/--help   — top-level usage; the help text routes the two
	//                 audiences (delegated agents vs human users) to
	//                 the right surface.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "exec":
			exec.Handle(os.Args[2:])
			return
		case "status":
			exec.HandleStatus(os.Args[2:])
			return
		case "resume":
			resume.Handle(os.Args[2:])
			return
		case "install":
			install.Handle(os.Args[2:])
			return
		case "uninstall":
			uninstall.Handle(os.Args[2:])
			return
		case "migrate":
			migrate.Handle(os.Args[2:])
			return
		case "jwk-init":
			jwkinit.Handle(os.Args[2:])
			return
		case "-h", "--help", "help":
			printTopLevelHelp()
			return
		case "-v", "--version", "version":
			fmt.Println(Version)
			return
		}
	}

	// Server mode: start HTTP server + pollers
	port := defaultPort
	host := defaultHost
	noBrowser := false

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--port":
			if i+1 < len(os.Args) {
				p, err := strconv.Atoi(os.Args[i+1])
				if err != nil {
					log.Fatalf("invalid port: %s", os.Args[i+1])
				}
				port = p
				i++
			}
		case "--host":
			if i+1 < len(os.Args) {
				host = os.Args[i+1]
				i++
			}
		case "--no-browser":
			noBrowser = true
		}
	}

	// Runmode dispatch (SKY-246 D2 wave 0): open the right backend
	// for the mode and wire the per-resource store bundle against
	// it. The dispatch wraps db.Open/db.Migrate so a misconfigured
	// TF_MODE=multi fails fast — without this guard, the local
	// SQLite file at ~/.triagefactory/triagefactory.db would be
	// created and migrated before the multi branch could reject.
	//
	// Multi mode is unreachable end-to-end until the v1 multi-tenant
	// epic (SKY-242) completes: every store needs to migrate to the
	// per-resource interface, and D7 needs to wire the Postgres
	// connection config. Until then, packages outside the converted
	// stores still call db.X(*sql.DB, ...) helpers that emit SQLite
	// SQL — pointed at Postgres they'd produce runtime errors. The
	// fatal here makes the unreachable state explicit instead of
	// surfacing later as a pile of confusing SQL failures.
	var (
		database *sql.DB
		stores   db.Stores
	)
	switch runmode.Current() {
	case runmode.ModeLocal:
		var err error
		database, err = db.Open()
		if err != nil {
			log.Fatalf("failed to open database: %v", err)
		}
		if err := db.Migrate(database, "sqlite3"); err != nil {
			log.Fatalf("failed to migrate database: %v", err)
		}
		// Fail fast if the migration's seeded UUIDs drifted from the
		// runmode constants — every team_id/creator_user_id DEFAULT
		// clause in the SQLite baseline embeds these literally, so a
		// mismatch would silently produce orphan rows.
		if err := db.AssertLocalSentinels(database); err != nil {
			log.Fatalf("%v", err)
		}
		stores = sqlitestore.New(database)
	case runmode.ModeMulti:
		// Multi-mode boot wires two Postgres pools against the same
		// server. admin (superuser) handles migrations + system-service
		// reads + tenant bootstrap; app (authenticator → tf_app)
		// handles RLS-active request handlers. The admin DSN comes in
		// whole via TF_DATABASE_URL; the app DSN reuses host/db/options
		// but swaps userinfo to authenticator + its own password (set
		// out-of-band by the postgres-postinit sidecar). Two passwords
		// by design — see CLAUDE.md and the postgres-postinit service
		// in docker-compose.yml.
		adminDSN := os.Getenv("TF_DATABASE_URL")
		if adminDSN == "" {
			log.Fatalf("TF_MODE=multi requires TF_DATABASE_URL")
		}
		authPassword := os.Getenv("TF_AUTHENTICATOR_PASSWORD")
		if authPassword == "" {
			log.Fatalf("TF_MODE=multi requires TF_AUTHENTICATOR_PASSWORD")
		}
		adminDB, err := sql.Open("pgx", adminDSN)
		if err != nil {
			log.Fatalf("open admin DB: %v", err)
		}
		applyPGPoolDefaults(adminDB)
		if err := adminDB.Ping(); err != nil {
			log.Fatalf("ping admin DB: %v", err)
		}
		appDSN, err := db.RewriteDSNCreds(adminDSN, "authenticator", authPassword)
		if err != nil {
			log.Fatalf("derive app DSN: %v", err)
		}
		appDB, err := sql.Open("pgx", appDSN)
		if err != nil {
			log.Fatalf("open app DB: %v", err)
		}
		applyPGPoolDefaults(appDB)
		if err := appDB.Ping(); err != nil {
			log.Fatalf("ping app DB: %v", err)
		}
		// Close the app pool on shutdown — the admin pool is deferred
		// via the shared `database` handle below. database/sql pools
		// don't auto-close on process exit, so leaving this unbound
		// would leak the pool's idle connections through any non-fatal
		// exit (signal-driven shutdown once that lands).
		defer appDB.Close()
		// Legacy *sql.DB consumers route to the admin pool for
		// system-service reads (no JWT-claims context).
		database = adminDB
		stores = pgstore.New(adminDB, appDB)

		// Best-effort startup cleanup of orphaned sandboxes from a
		// prior hard-crashed TF process. Sweeps /var/run/netns and
		// $TMPDIR for tf-* netns + bundle dirs. Never fatal — failure
		// here just means orphaned resources stick around until the
		// next boot or a manual cleanup.
		if err := sandbox.ReapOrphans(context.Background()); err != nil {
			log.Printf("sandbox: reap orphans at boot: %v", err)
		}
	default:
		log.Fatalf("unknown runmode: %v", runmode.Current())
	}
	defer database.Close()

	// Boot-time deployment settings: instance_config holds the small
	// remainder of process-wide state (server port, takeover dir).
	// Local-mode only — the table doesn't exist in the Postgres
	// baseline because hosted multi-mode uses env vars for these.
	// The takeover dir is plumbed to the Server and Spawner
	// constructors so neither has to read settings on every handler
	// call. The stored port is surfaced to the settings GET response
	// (the actual bind still wins from --port at boot).
	var (
		storedPort        int
		storedTakeoverDir string
	)
	if runmode.Current() == runmode.ModeLocal {
		if err := database.QueryRowContext(context.Background(),
			`SELECT server_port, server_takeover_dir FROM instance_config WHERE id = 1`,
		).Scan(&storedPort, &storedTakeoverDir); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Fatalf("read instance_config: %v", err)
		}
	}
	// Default storedPort to the binary's defaultPort when the row is
	// missing or holds the zero value — that's what the Settings GET
	// response should render in its server_port input. Belt-and-
	// suspenders for pre-seed DBs and the multi-mode branch that
	// skips the read entirely.
	if storedPort == 0 {
		storedPort = defaultPort
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	// Display host: keep "localhost" in the browser-facing URL when bound
	// to loopback (prettier, and what users expect), but show the actual
	// bind host when overridden so it's obvious where the server is
	// reachable.
	displayHost := host
	if host == "127.0.0.1" || host == "" {
		displayHost = "localhost"
	}
	browserURL := fmt.Sprintf("http://%s:%d", displayHost, port)
	fmt.Printf("Triage Factory running at %s\n", browserURL)

	// One-shot PATH hint. The `triagefactory resume` subcommand only
	// works from any terminal once the binary's on PATH; nudge the
	// user toward `triagefactory install` if it isn't. Best-effort.
	install.HintIfMissing()

	if !noBrowser {
		openBrowser(browserURL)
	}

	srv := server.New(database, stores, storedTakeoverDir, storedPort)

	// Local-mode deploy config: publicURL is the local address, HMAC
	// key is ephemeral (only needs to survive the registration window,
	// not across restarts). Multi mode populates deployCfg inside
	// SetAuthDeps below, so this block is local-only.
	if runmode.Current() == runmode.ModeLocal {
		var hmacKey [32]byte
		if _, err := cryptorand.Read(hmacKey[:]); err != nil {
			log.Fatalf("generate local HMAC key: %v", err)
		}
		srv.SetDeployConfig(browserURL, hmacKey)
	}

	// Multi-mode auth wiring. The verifier blocks on the initial JWKS
	// fetch (see verify.NewVerifier docstring), so GoTrue must be
	// reachable before TF boots — docker-compose handles this via
	// `depends_on: gotrue: { condition: service_healthy }`. The
	// session reaper goroutine spawned inside SetAuthDeps inherits
	// ctxBoot; it lives for the binary's lifetime (the binary has no
	// top-level cancel today).
	if runmode.Current() == runmode.ModeMulti {
		ctxBoot := context.Background()

		// Validate TF_PUBLIC_URL up front. SetAuthDeps derives the
		// secureCookies flag from `strings.HasPrefix(publicURL, "https://")`
		// — an empty or typo'd URL would silently land in the non-secure
		// branch and emit OAuth-state cookies without the Secure flag.
		// Failing fast here is much louder than the runtime cookie-flag
		// drift, and the OAuth redirect URLs also need a real scheme+host.
		publicURL := os.Getenv("TF_PUBLIC_URL")
		if err := validateHTTPURL("TF_PUBLIC_URL", publicURL); err != nil {
			log.Fatalf("%v", err)
		}

		// SKY-345: read the signup join policy. Unset → personal-org-on-signup
		// (right default for hosted SaaS + unconfigured self-hosts). Any
		// unknown value fatals here so a typo in .env (`personal_org_signup`
		// instead of `personal-org-on-signup`) surfaces loudly at boot
		// instead of silently degrading to a wrong-default behavior on
		// every fresh signup.
		if err := runmode.InitJoinPolicyFromEnv(); err != nil {
			log.Fatalf("%v", err)
		}

		verifier, err := verify.NewVerifier(
			ctxBoot,
			os.Getenv("TF_GOTRUE_JWKS_URL"),
			os.Getenv("TF_GOTRUE_ISSUER"),
			"authenticated", // GoTrue's standard audience claim
		)
		if err != nil {
			log.Fatalf("build verifier: %v", err)
		}

		sessionKey, err := sessions.LoadKeyFromEnv(sessions.EnvSessionEncryptionKey)
		if err != nil {
			log.Fatalf("load session encryption key: %v", err)
		}
		cookieKey, err := sessions.LoadKeyFromEnv(sessions.EnvCookieSecret)
		if err != nil {
			log.Fatalf("load cookie secret: %v", err)
		}

		sessionStore := sessions.NewStore(database, sessionKey)
		if err := srv.SetAuthDeps(
			ctxBoot,
			verifier,
			sessionStore,
			os.Getenv("TF_GOTRUE_URL"),
			publicURL,
			cookieKey,
		); err != nil {
			log.Fatalf("wire auth deps: %v", err)
		}
	}

	distFS, err := frontendDist()
	if err != nil {
		log.Fatalf("failed to load embedded frontend: %v", err)
	}
	srv.SetStatic(distFS)

	// Clean up any orphaned worktrees from crashed runs. taken_over runs
	// are preserved at the ~/.claude/projects level so the user can still
	// resume their takeover sessions after a binary restart.
	//
	// On query error we still sweep worktree dirs and prune bare repos
	// (those leaks compound fast — each can be GBs), but skip ALL
	// ~/.claude/projects deletions: without the preserve set we can't
	// distinguish a taken-over run's session JSONL from a regular
	// orphan, and silently nuking a JSONL would break the user's ability
	// to resume.
	//
	// Local mode only: the preserve set is keyed by the synthetic
	// sentinel org, which has no real-tenant rows in multi mode — and
	// the `triagefactory resume` UX itself is local-only. In multi
	// mode we still want the worktree-dir + bare-repo sweep (those
	// leaks are not mode-specific), but we skip ~/.claude/projects
	// cleanup entirely so we don't clobber any real-tenant takeover
	// JSONLs we'd be unable to identify without a meaningful preserve
	// set.
	if runmode.Current() == runmode.ModeLocal {
		preserveIDs, err := stores.AgentRuns.ListTakenOverIDsSystem(context.Background(), runmode.LocalDefaultOrgID)
		if err != nil {
			log.Printf("[server] WARNING: failed to load taken_over run ids — sweeping worktree dirs but skipping ~/.claude/projects cleanup to avoid clobbering active takeover sessions: %v", err)
			worktree.CleanupWithOptions(worktree.CleanupOptions{SkipClaudeProjectCleanup: true})
		} else {
			preserveSet := make(map[string]bool, len(preserveIDs))
			for _, id := range preserveIDs {
				preserveSet[id] = true
			}
			worktree.CleanupWithOptions(worktree.CleanupOptions{PreserveClaudeProjectFor: preserveSet})
		}
	} else {
		worktree.CleanupWithOptions(worktree.CleanupOptions{SkipClaudeProjectCleanup: true})
	}

	// events_catalog is seeded by the v1.11.0 baseline migration in both
	// backends — no boot-time seed call needed. New event types ship via
	// a new forward migration. Prompts are seeded inside seedDefaultPrompts
	// before EventHandlers.Seed runs so the FK from event_handlers.prompt_id
	// → prompts.id resolves on the trigger rows.
	//
	// Populate users.github_username before seeding event handlers so the
	// SQLite Seed substitution sees the local user's login when it wires
	// allowlist placeholders on shipped predicates.
	if err := bootstrapLocalGitHubIdentity(stores.Users, stores.Secrets); err != nil {
		log.Printf("[bootstrap] users.github_username: %v (continuing — Settings will capture on next save)", err)
	}
	if err := bootstrapLocalJiraIdentity(stores.Users, stores.Secrets); err != nil {
		log.Printf("[bootstrap] users.jira_identity: %v (continuing — Settings will capture on next save)", err)
	}
	// Local mode only: shipped prompts + handlers materialize against
	// the synthetic (LocalDefaultOrg, LocalDefaultTeamID) pair, neither
	// of which has a real row in multi-mode Postgres — the event_handlers
	// insert would FK-fail on first boot. Multi-mode tenants get the
	// shipped content seeded by the org-create / team-create flows
	// (D14), which run against real orgs and teams.
	if runmode.Current() == runmode.ModeLocal {
		seedDefaultPrompts(stores.Prompts, stores.EventHandlers)
	}

	// Bootstrap the local-mode agent identity (SKY-260 D-Agent). One
	// agents row + one team_agents row for the synthetic LocalDefaultOrg
	// / LocalDefaultTeamID pair. Idempotent (INSERT OR IGNORE) — re-runs
	// across boots leave existing rows intact, preserving any user-
	// disable on team_agents.enabled.
	//
	// Fatal on failure: post-SKY-261 the agents row is load-bearing for
	// the entire claim flow (stampAgentClaim's GetForOrg, the drain
	// path's claim_changed guard, runs.actor_agent_id stamping). The
	// idempotent INSERT means the only legitimate failure mode is a
	// DB connection issue — and Migrate() above already fatals on
	// that. Continuing past a bootstrap failure produces a silently-
	// broken auto-delegation state where the user wouldn't see an
	// error, just notice things never fire. Better to surface the
	// failure at startup.
	//
	// Local mode only: multi-mode bootstraps a real agents row per org
	// via the admin org-create flow (SKY-257). There is no synthetic
	// org in multi mode for this row to attach to.
	if runmode.Current() == runmode.ModeLocal {
		if err := db.BootstrapLocalAgent(context.Background(), stores); err != nil {
			log.Fatalf("[bootstrap] local agent: %v (auto-delegation depends on this; refusing to start)", err)
		}
	}

	// Auto-import Claude Code skill files as prompts. Local mode
	// only: the importer's store calls run as the boot process with
	// no user identity, which works against SQLite (no RLS) but
	// would fail against Postgres tf_app for lack of claims. Multi-
	// mode users will import prompts via the request-driven CRUD
	// surface, where the handler has claims; auto-import on boot
	// doesn't make sense there anyway because SKILL.md files live
	// on the user's machine, not the server's.
	if runmode.Current() == runmode.ModeLocal {
		skills.ImportAll(context.Background(), database, stores.Prompts)
	}

	// Event bus — central pub/sub replacing direct callbacks
	bus := eventbus.New()
	// Let the GitHub webhook receiver publish verified deliveries.
	srv.SetEventBus(bus)

	wsHub := srv.WSHub()

	// Wire the worktree clone-result callback before any bootstrap or
	// lazy-clone path can fire. EnsureBareClone (and its private
	// equivalent used by CreateForPR / createBranchWorktreeAt) invokes
	// this on every attempt; we use it to stamp repo_profiles with the
	// outcome and broadcast a websocket event so the Repos page updates
	// live. Failures get an SSH preflight to classify whether the SSH
	// side is the cause — that drives the per-row CTA on the frontend
	// ("Fix in Settings" for SSH issues, raw stderr otherwise).
	//
	// Local mode only: the callback body hardcodes LocalDefaultOrgID
	// for the row-stamp + WS broadcast. Multi-mode clone-status fan-out
	// requires per-request orgID threading through worktree's callback
	// surface; that's a follow-up. For now multi-mode just doesn't
	// surface live clone-status updates — the underlying clone still
	// happens, only the UI feedback is missing.
	if runmode.Current() == runmode.ModeLocal {
		worktree.SetOnCloneResult(func(owner, repo string, cloneErr error) {
			if cloneErr == nil {
				if err := stores.Repos.UpdateCloneStatusSystem(context.Background(), runmode.LocalDefaultOrgID, owner, repo, "ok", "", ""); err != nil {
					log.Printf("[clone-status] update %s/%s ok: %v", owner, repo, err)
				}
				// Scoped to the local sentinel org — the upstream UpdateCloneStatusSystem
				// call above stamps the same org id, so the broadcast surface matches
				// the row's owning tenant. Multi-mode clone-status fan-out is a
				// separate concern (the callback today only fires from local-mode
				// paths).
				wsHub.Broadcast(websocket.Event{
					Type:  "repo_profile_updated",
					OrgID: runmode.LocalDefaultOrgID,
					Data: map[string]any{
						"id":           owner + "/" + repo,
						"clone_status": "ok",
					},
				})
				return
			}

			log.Printf("[clone-status] %s/%s clone failed: %v", owner, repo, cloneErr)

			kind := "other"
			orgSet, oErr := stores.Orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
			if oErr != nil {
				log.Printf("[clone-status] %s/%s load org settings to classify: %v (defaulting to kind=other)", owner, repo, oErr)
			} else if orgSet.GitHubCloneProtocol == "ssh" {
				// Use the configured GitHub host so GHE installs probe
				// the right SSH endpoint, not github.com. Falls back to
				// git@github.com when the URL is empty/unparseable.
				creds, _ := integrations.Load(context.Background(), stores.Secrets, runmode.LocalDefaultOrgID)
				sshHost := worktree.SSHHostFromBaseURL(creds.GitHubURL)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				if perr := worktree.CachedPreflightSSH(ctx, sshHost); perr != nil {
					kind = "ssh"
					log.Printf("[clone-status] %s/%s SSH preflight against %s also failed → kind=ssh: %v", owner, repo, sshHost, perr)
				} else {
					log.Printf("[clone-status] %s/%s SSH preflight against %s passed → kind=other (clone error is on the git side)", owner, repo, sshHost)
				}
				cancel()
			}

			if err := stores.Repos.UpdateCloneStatusSystem(context.Background(), runmode.LocalDefaultOrgID, owner, repo, "failed", cloneErr.Error(), kind); err != nil {
				log.Printf("[clone-status] update %s/%s failed: %v", owner, repo, err)
			}
			wsHub.Broadcast(websocket.Event{
				Type:  "repo_profile_updated",
				OrgID: runmode.LocalDefaultOrgID,
				Data: map[string]any{
					"id":               owner + "/" + repo,
					"clone_status":     "failed",
					"clone_error":      cloneErr.Error(),
					"clone_error_kind": kind,
				},
			})
		})
	}

	// Subscriber: WS broadcaster — forwards ALL events to the frontend.
	//
	// Classified as system-service for the SKY-310 / D9a bus profiles:
	// the WS hub itself is not yet per-(user, org) scoped. D9b lifts
	// the hub to a per-org fanout, at which point this becomes either
	// an org-scoped SubscribeFor or a system-service that dispatches
	// per-OrgID. For now the local-mode promise holds because every
	// published event carries LocalDefaultOrgID and there's one hub.
	bus.Subscribe(eventbus.Subscriber{
		Name: "ws-broadcast",
		Handle: func(evt domain.Event) {
			// Forward the bus's per-event OrgID so the WS hub's
			// per-connection filter scopes the fanout to the right
			// tenant. System events (evt.OrgID == "") propagate as
			// system-wide broadcasts that deliver everywhere.
			wsHub.Broadcast(websocket.Event{
				Type:  "event",
				OrgID: evt.OrgID,
				Data:  evt,
			})
			// Also send the legacy "tasks_updated" for backward compat,
			// scoped to the same tenant as the originating event.
			if evt.EventType == domain.EventSystemPollCompleted {
				wsHub.Broadcast(websocket.Event{
					Type:  "tasks_updated",
					OrgID: evt.OrgID,
					Data:  map[string]any{},
				})
			}
		},
	})

	// Start AI scoring runner
	// Profile gate — scorer waits for this before running
	profileGate := repoprofile.NewProfileGate(database)

	// Declare eventRouter early so the scorer callback can reference it.
	// Actual initialization happens below after the spawner is created.
	var eventRouter *routing.Router

	scorer := ai.NewManager(database, stores.Scores, stores.Entities, ai.RunnerCallbacks{
		OnScoringStarted: func(orgID string, taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type:  "scoring_started",
				OrgID: orgID,
				Data:  map[string]any{"task_ids": taskIDs},
			})
		},
		OnScoringCompleted: func(orgID string, taskIDs []string) {
			wsHub.Broadcast(websocket.Event{
				Type:  "scoring_completed",
				OrgID: orgID,
				Data:  map[string]any{"task_ids": taskIDs},
			})
			// Post-scoring re-derive: check deferred triggers whose
			// min_autonomy_suitability threshold the scored tasks now meet.
			// Runs async so it doesn't block the scorer from clearing its
			// running flag and handling subsequent Trigger() calls.
			if eventRouter != nil {
				go eventRouter.ReDeriveAfterScoring(orgID, taskIDs)
			}
		},
		OnTasksSkipped: func(orgID string, skipped, total int) {
			toast.Warning(wsHub, orgID, fmt.Sprintf("AI scoring: %d of %d tasks skipped this cycle", skipped, total))
		},
		OnError: func(orgID string, err error) {
			toast.Error(wsHub, orgID, fmt.Sprintf("AI scoring cycle aborted: %v", err))
		},
	})
	scorer.SetProfileGate(profileGate.Ready)
	srv.SetScorerTrigger(scorer.Trigger)
	log.Println("[ai] scorer manager ready (per-org runners, model: haiku)")

	// Subscriber: scorer trigger — only reacts to poll-complete sentinels.
	// Per-org pollers emit one sentinel per (org, source); the Manager
	// routes each to that org's Runner so a slow scoring cycle on one
	// tenant doesn't block others' min_autonomy_suitability triggers.
	bus.Subscribe(eventbus.Subscriber{
		Name:   "scorer",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			scorer.Trigger(evt.OrgID)
		},
	})

	// Project classifier (SKY-220): per-poll, classify any newly-
	// discovered entities against existing projects via per-project
	// Haiku quorum vote. Sticky — only fires on entities with
	// classified_at IS NULL, so re-polls don't re-classify.
	classifier := projectclassify.NewRunner(database, stores.Entities, stores.Projects, stores.Orgs)
	classifier.Start()
	log.Println("[classify] project classifier started (model: haiku)")
	// System-service profile (D9a): kicked by any tenant's poll
	// completion; the classifier rotates through orgs internally.
	bus.Subscribe(eventbus.Subscriber{
		Name:   "classifier",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			classifier.Trigger()
		},
	})

	// Poller manager — uses event bus instead of direct callbacks.
	// Poll errors are toasted with per-source time-based throttling: the
	// poller fires OnError on every failure (raw signal), but we only
	// refresh the user-facing toast every errorToastMinInterval. Without
	// throttling, a persistent failure (expired PAT, network outage) would
	// generate a sticky error toast every poll cycle (default 5m) until
	// the user manually dismissed each one — badly spammy on the UI.
	const errorToastMinInterval = 5 * time.Minute
	var (
		errorThrottleMu sync.Mutex
		lastErrorToast  = map[string]time.Time{}
	)
	// GitHub credential resolver for the poller — resolves an App
	// installation token (tier 1) or the org PAT (tier 3) per cycle, per
	// installation. Its own token cache: installation tokens
	// carry a TTL and the cache treats a token inside the expiry guard as a
	// miss, so the poller re-mints on its own schedule without sharing the
	// server's cache.
	pollerGHResolver := ghclient.NewResolver(stores.Secrets, stores.GitHubApps, stores.Orgs, stores.Agents, nil)
	pollerMgr := poller.NewManager(database, bus, stores.Users, stores.Tasks, stores.Entities, stores.Repos, stores.Orgs, stores.Teams, stores.JiraStatusRules, stores.TeamGitHubGroups, stores.Secrets, stores.GitHubApps, pollerGHResolver)
	pollerMgr.OnError = func(source, orgID string, err error) {
		// Throttle key includes orgID so a chronic failure on one tenant
		// doesn't suppress a fresh failure on another. Process-level
		// errors (ListActiveSystem) pass orgID="" and throttle together
		// per source — that's still the right behavior for "Jira API
		// is down" style spam.
		throttleKey := source + ":" + orgID
		errorThrottleMu.Lock()
		if last, ok := lastErrorToast[throttleKey]; ok && time.Since(last) < errorToastMinInterval {
			errorThrottleMu.Unlock()
			return
		}
		lastErrorToast[throttleKey] = time.Now()
		errorThrottleMu.Unlock()

		label := "Jira"
		if source == "github" {
			label = "GitHub"
		}
		toast.ErrorTitled(wsHub, orgID, label, fmt.Sprintf("Poll failed: %v", err))
	}

	// Create spawner once — credentials are hot-swapped in place
	spawner := delegate.NewSpawner(database, stores, nil, wsHub, "", storedTakeoverDir)
	// Hand the full Stores bundle so the sandbox-branch agenthost
	// daemon can serve every routing-sensitive RPC the agent's
	// `triagefactory exec` invocations send. Local-mode + non-sandbox
	// paths never read this back; nil-safe inside the spawner.
	spawner.SetStores(stores)
	srv.SetSpawner(spawner)

	// SKY-220: wire the classifier wait into the spawner's setup path.
	// Before reading entity.project_id for KB injection, the spawner
	// blocks until classified_at is set (or DefaultWaitTimeout elapses).
	// projectclassify.WaitFor triggers the runner on entry to wake it up
	// even if no post-poll cycle has fired for this entity yet.
	spawner.SetWaitForClassification(func(ctx context.Context, entityID string) {
		projectclassify.WaitFor(ctx, database, classifier, entityID, projectclassify.DefaultWaitTimeout)
	})

	// Curator runtime (SKY-216) — per-project chat sessions. Sweep
	// stranded curator turns from a previous process. A binary
	// restart kills every per-project curator goroutine + agentproc
	// subprocess in this process, so any `queued` or `running` row
	// is by definition stranded — cancelling it makes the user
	// re-send rather than wait for a delayed mystery reply. In
	// multi mode this affects every tenant whose chat was in-flight
	// at restart time; documented as intentional. Multi-pod per-org
	// sharding would let us scope this per-pod, but pod sharding
	// doesn't exist (single-pod multi-mode in v1).
	//
	// The model arg below is empty until config loads;
	// curator.UpdateCredentials hot-swaps the same way Spawner does.
	if n, err := stores.Curator.CancelOrphanedNonTerminalRequests(context.Background()); err != nil {
		log.Printf("[curator] sweep stranded turns: %v", err)
	} else if n > 0 {
		log.Printf("[curator] cancelled %d stranded turn(s) from prior process", n)
	}
	curatorRuntime := curator.New(database, stores, wsHub, "")
	srv.SetCurator(curatorRuntime)

	// Knowledge-base file watcher — fires `project_knowledge_updated`
	// over the websocket whenever the curator (or anything else)
	// touches a file under <projectsRoot>/<id>/knowledge-base/. The
	// frontend Knowledge panel listens and refetches, so files appear
	// in the UI as the agent writes them mid-turn. Failure here is
	// non-fatal — the panel still works, just without live updates.
	//
	// resolveOrgForProject lets the watcher stamp each broadcast with
	// the project's owning org so the hub's per-connection filter
	// keeps the event scoped to that tenant. Uses the admin-pool
	// ResolveOrgSystem variant — the watcher fires from a fs-events
	// goroutine with no claims context.
	//
	// Returning "" tells the watcher to drop the broadcast rather
	// than fall back to a system-wide fanout (which the hub would
	// deliver to every connected tenant, leaking the update cross-
	// tenancy). Both branches below — lookup error and no-row — log
	// on this side so the failure is visible without the watcher
	// having to know why.
	resolveOrgForProject := func(projectID string) string {
		orgID, err := stores.Projects.ResolveOrgSystem(context.Background(), projectID)
		if err != nil {
			log.Printf("[kbwatcher] resolve org for project %s: %v (dropping live update)", projectID, err)
			return ""
		}
		if orgID == "" {
			log.Printf("[kbwatcher] no org for project %s — stale dir or unresolved row (dropping live update)", projectID)
			return ""
		}
		return orgID
	}
	if root, err := curator.ProjectsRoot(); err != nil {
		log.Printf("[kbwatcher] resolve projects root: %v (live KB updates disabled)", err)
	} else if _, err := curator.NewKnowledgeWatcher(wsHub, root, resolveOrgForProject); err != nil {
		log.Printf("[kbwatcher] start: %v (live KB updates disabled)", err)
	}

	// Event router — records events, creates/bumps tasks, auto-delegates on
	// matching triggers, runs inline close checks. Also handles post-scoring
	// re-derive via the scorer callback wired above.
	eventRouter = routing.NewRouter(stores.Prompts, stores.EventHandlers, stores.Agents, stores.TeamAgents, stores.Users, stores.Tasks, stores.AgentRuns, stores.Entities, stores.PendingFirings, stores.Events, stores.Orgs, stores.Teams, spawner, scorer, wsHub)
	// System-service profile (D9a): the router branches on evt.OrgID
	// itself when persisting and fanning out — every event flows here
	// regardless of tenant. Multi-mode handlers thread the orgID into
	// the per-handler store calls.
	bus.Subscribe(eventbus.Subscriber{
		Name:   "router",
		Filter: []string{"github:", "jira:"},
		Handle: eventRouter.HandleEvent,
	})

	// Wire the queue drainer. Spawner calls router.DrainEntity from each
	// auto-run terminal so queued firings progress without their own
	// trigger event. Has to be set post-construction because router and
	// spawner reference each other (spawner.Delegate ← router; router.
	// DrainEntity ← spawner). Same pattern UpdateCredentials uses.
	spawner.SetQueueDrainer(eventRouter)

	// Periodic drain sweeper — safety net for queues stuck on transient
	// validation/fire errors. notifyDrainer only triggers drains on
	// auto-run terminals; if nothing's running, nothing wakes up the
	// queue. The sweep tick re-attempts pending firings every 30s.
	// Background context: the binary doesn't have a top-level cancel
	// today, so the goroutine lives for the process lifetime.
	go eventRouter.RunDrainSweeper(context.Background(), 30*time.Second)

	// Tracks per-source "announce next poll completion as a toast". Set when
	// a config change triggers a poller restart; cleared after the first
	// post-restart completion fires the toast. Prevents every-minute spam
	// while still giving users explicit feedback that their config took
	// effect.
	var (
		announceMu      sync.Mutex
		announcePending = map[string]bool{}
	)
	setAnnouncePending := func(source string) {
		announceMu.Lock()
		announcePending[source] = true
		announceMu.Unlock()
	}
	shouldAnnounce := func(source string) bool {
		announceMu.Lock()
		defer announceMu.Unlock()
		if announcePending[source] {
			announcePending[source] = false
			return true
		}
		return false
	}

	// GitHub changed: invalidate profiles → stop all → re-profile → restart all.
	//
	// Gated to local mode. The callback wires process-global state
	// (ghClient, curator credentials, profiler) shaped for a
	// single-tenant binary; in multi-mode the same work happens
	// per-tenant inside the request handlers and on first
	// delegation per repo. Additionally, the integrations.Load call
	// below would fail in Postgres without request.jwt.claims set
	// (SecretStore vault_* enforces org_id == tf.current_org_id()),
	// and the callback fires from a goroutine with no claims
	// context.
	//
	// TODO: multi-mode per-tenant credential refresh — add a
	// SystemGet-style SecretStore variant or plumb userID through
	// the callback for SyntheticClaimsWithTx. Same follow-up as the
	// startJira TODO in internal/poller/manager.go.
	srv.SetOnGitHubChanged(func(orgID string) {
		log.Println("[server] GitHub config changed, full restart...")
		setAnnouncePending("github")
		setAnnouncePending("jira")

		// Don't Invalidate the profile gate. Scoring no longer reads
		// repo profiles (LLM repo-match was removed when lazy Jira
		// worktrees landed), so the gate has no consumer; flipping it
		// back to false in the GitHub-disabled branch — which doesn't
		// Signal again — would silently freeze scoring forever.
		pollerMgr.StopAll()

		if runmode.Current() != runmode.ModeLocal {
			log.Println("[server] GitHub changed: multi-mode skips process-global refresh (per-tenant work happens in request handlers)")
			return
		}

		ctx := context.Background()
		creds, _ := integrations.Load(ctx, stores.Secrets, orgID)
		model := resolveAIModelForOrg(ctx, stores, orgID)

		if creds.GitHubPAT != "" && creds.GitHubURL != "" {
			ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
			spawner.UpdateCredentials(ghClient, model)
			curatorRuntime.UpdateCredentials(model)
			srv.SetGitHubClient(ghClient)

			// Re-profile, then signal ready and restart all pollers
			go func() {
				profiler := repoprofile.NewProfiler(ghClient, database, stores.Repos, stores.Orgs, wsHub)
				if err := profiler.Run(context.Background(), true); err != nil {
					log.Printf("[repoprofile] profiling failed: %v", err)
				}
				profileGate.Signal()
				pollerMgr.RestartAll(context.Background(), orgID)
				scorer.Trigger(orgID)
				// Bare-clone bootstrap is best-effort and local-mode-shaped:
				// it reads repos under the synthetic sentinel org, which
				// has no rows in multi mode. The lazy-clone path inside
				// CreateForPR / CreateForBranch handles multi mode on
				// first delegation per repo per org.
				if runmode.Current() == runmode.ModeLocal {
					bootstrapBareClones(database, stores.Repos)
				}
			}()
		} else {
			spawner.UpdateCredentials(nil, "")
			curatorRuntime.UpdateCredentials("")
			srv.SetGitHubClient(nil)
			pollerMgr.RestartAll(ctx, orgID)
		}

		// Also refresh Jira client in case it's configured
		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT))
		} else {
			srv.SetJiraClient(nil)
		}
	})

	// Jira changed: restart only the Jira poller. Same local-mode
	// gate as onGitHubChanged — process-global Jira client + the
	// SecretStore claims requirement make this a local-only path.
	srv.SetOnJiraChanged(func(orgID string) {
		log.Println("[server] Jira config changed, restarting Jira poller...")
		setAnnouncePending("jira")

		if runmode.Current() != runmode.ModeLocal {
			log.Println("[server] Jira changed: multi-mode skips process-global refresh")
			return
		}

		ctx := context.Background()
		creds, _ := integrations.Load(ctx, stores.Secrets, orgID)

		pollerMgr.RestartJira(ctx, orgID)

		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT))
		} else {
			srv.SetJiraClient(nil)
		}
	})

	// Subscriber: track Jira/GitHub poll completions.
	// Jira: gates /api/jira/stock so it knows when snapshots are ready.
	// Both: surface a one-shot "first poll complete after config change"
	// toast so users can see their settings change actually took effect.
	//
	// System-service profile (D9a): poll-completed sentinels are
	// per-source, not per-tenant, in local mode. D9c's per-org loops
	// will emit one sentinel per (org, source) and the announce/stock
	// state machine here is org-agnostic — it just cares that *a* poll
	// completed.
	bus.Subscribe(eventbus.Subscriber{
		Name:   "poll-tracker",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) {
			if evt.EventType != domain.EventSystemPollCompleted {
				return
			}
			var meta struct {
				Source    string `json:"source"`
				StartedAt int64  `json:"started_at"`
				Entities  int    `json:"entities"`
			}
			if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
				log.Printf("[poll-tracker] warning: failed to parse poll completion metadata: %v; raw metadata=%q", err, evt.MetadataJSON)
				return
			}
			if meta.Source == "jira" {
				// Pass the poll's started_at so MarkJiraPollComplete can ignore
				// stale sentinels from pre-restart poll goroutines that finish
				// late — RestartJira doesn't cancel in-flight RefreshJira calls.
				// A missing field yields StartedAt=0; pass a zero time.Time so
				// MarkJiraPollComplete treats it as "unknown generation" and
				// accepts it rather than getting stuck on {status:"polling"}.
				var startedAt time.Time
				if meta.StartedAt != 0 {
					startedAt = time.Unix(0, meta.StartedAt)
				}
				srv.MarkJiraPollComplete(startedAt)
			}
			if shouldAnnounce(meta.Source) {
				label := "GitHub"
				if meta.Source == "jira" {
					label = "Jira"
				}
				toast.Info(wsHub, evt.OrgID, fmt.Sprintf(
					"First %s poll complete — %d %s tracked",
					label, meta.Entities, pluralize(meta.Entities, "entity", "entities"),
				))
			}
		},
	})

	// Initial start with current credentials. Local mode by design — multi-mode
	// users get GitHub identity per-org via the D14 admin UI, not via this
	// keychain-driven boot path. Hard-gating on the runmode lets us keep using
	// the local sentinel here without it leaking into multi-mode startup:
	// reading the synthetic sentinel org's secrets in multi mode would
	// return zero values and we'd hand an unauthenticated GitHub client to
	// every downstream subsystem, then quietly log per-cycle 401s.
	if runmode.Current() == runmode.ModeLocal {
		ctx := context.Background()
		orgID := runmode.LocalDefaultOrgID
		creds, _ := integrations.Load(ctx, stores.Secrets, orgID)
		repoCount, _ := stores.Repos.CountConfiguredSystem(ctx, orgID)
		model := resolveAIModelForOrg(ctx, stores, orgID)

		if creds.GitHubPAT != "" && creds.GitHubURL != "" && repoCount > 0 {
			ghClient := ghclient.NewClient(creds.GitHubURL, creds.GitHubPAT)
			spawner.UpdateCredentials(ghClient, model)
			curatorRuntime.UpdateCredentials(model)
			srv.SetGitHubClient(ghClient)
			log.Printf("[delegate] spawner ready (%d repos configured)", repoCount)

			// Profile repos, then signal ready, start pollers, and trigger scoring
			go func() {
				profiler := repoprofile.NewProfiler(ghClient, database, stores.Repos, stores.Orgs, wsHub)
				if err := profiler.Run(context.Background(), false); err != nil {
					log.Printf("[repoprofile] initial profiling failed: %v", err)
				}
				profileGate.Signal()
				pollerMgr.RestartAll(context.Background(), orgID)
				scorer.Trigger(orgID)
				bootstrapBareClones(database, stores.Repos)
			}()
		} else {
			// Not fully configured — start pollers immediately (may be empty)
			pollerMgr.RestartAll(ctx, orgID)
		}

		if creds.JiraPAT != "" && creds.JiraURL != "" {
			srv.SetJiraClient(jira.NewClient(creds.JiraURL, creds.JiraPAT))
		}
	}

	if err := srv.ListenAndServe(addr); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
