package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// Leaving the managed class — the mirror of the bind ceremony next door.
//
// The deployment App is something a workspace CHOOSES. The bind is the only
// path into the class, and it lands on an admin's explicit Connect click; this
// file is the only path out, and it lands on an explicit Disconnect. What it
// protects is the one fact the rest of the managed machinery reads: a live
// installation row means "this workspace rides the deployment App". The static
// webhook receiver routes a delivery by that row, the cadence pass refreshes
// every live row, and the resolver mints tokens from them — none of them
// consult the credential class. So a workspace whose class moved to pat or
// byo_app while its rows stayed live would keep receiving, and acting on, the
// deployment App's deliveries under a credential class that says it has no App
// at all. The rows and the class must never disagree, and two things hold that:
//
//   - the disconnect verbs, which soft-remove the rows and reset the class in
//     one motion under the App-registration lock;
//   - the door guards, one predicate called from every other credential bind
//     (the PAT bind, BYO registration start and callback, BYO import), which
//     refuse a managed workspace that still holds a live row and name the
//     disconnect as the way out.
//
// Nothing here uninstalls anything on GitHub. The grant is edited on GitHub's
// installation page, so after a disconnect the installation persists there
// unbound — the same ordinary state a GitHub-initiated install is in — and
// Connect re-binds it.

// managedInTheWayMessage is the one sentence every door says to a managed
// workspace. Same rule on four routes is the same rule, so it is spelled once.
const managedInTheWayMessage = "This workspace is connected through the deployment's GitHub App. " +
	"Disconnect it in Workspace Settings first."

// unknownGitHubClassMessage is what every door says to a workspace whose class
// this build cannot name. Spelled once for the same reason as the sentence
// above: the launch page and the JSON doors owe the caller the same answer.
const unknownGitHubClassMessage = "This workspace's GitHub credential is managed in a way this version doesn't recognize."

// errOrgManagedInTheWay is the door guard's refusal as an error, for the one
// door (the registration launch) whose refusals travel as sentinel errors from
// the manifest builder to the page renderer, which puts the sentence above on
// the page.
var errOrgManagedInTheWay = errors.New("org rides the deployment app with a live installation")

// managedInstallationsInTheWay is the door guard's predicate: does this
// workspace ride the deployment App with at least one live installation row?
// That, and not the class alone, is what the guard is about — the hole it
// closes is a live row under a different class, so a managed workspace whose
// rows have all been removed (an uninstall reported by webhook, say) is free to
// bind a PAT and become the plain PAT workspace it already effectively is.
//
// An unknown class is refused as an error rather than read as "not managed":
// the caller cannot know whether rows under a class this build cannot name are
// in the way, and the doors already treat an unknown class as a reason not to
// write. System reads: every caller holds an admin-authorized orgID, and the
// installation read is the same claims-free one the resolver uses.
func (s *Server) managedInstallationsInTheWay(ctx context.Context, orgID string) (bool, error) {
	class, err := s.githubCredentialClass(ctx, orgID)
	if err != nil {
		return false, err
	}
	if class != domain.GitHubCredentialClassManagedApp {
		return false, nil
	}
	rows, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		return false, fmt.Errorf("list managed installations: %w", err)
	}
	return len(rows) > 0, nil
}

// refuseManagedInTheWay is the door guard as the JSON doors call it: it writes
// the refusal and reports whether it did, so a door reads as
//
//	if s.refuseManagedInTheWay(w, ctx, orgID) { return }
//
// at both the advisory and the authoritative (under-lock) evaluation. Both
// evaluations owe the caller the same sentence, which is why there is one
// helper and not two spellings of the check. A managed workspace is a 409: the
// route is real and the request well-formed, and what is in the way is state
// the caller can change. An unknown class is a 409 too, for the same reason the
// PAT bind gives it one — this door cannot write under a class it cannot name.
func (s *Server) refuseManagedInTheWay(w http.ResponseWriter, ctx context.Context, orgID string) bool {
	inTheWay, err := s.managedInstallationsInTheWay(ctx, orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			githubAppLog.Error("unknown github credential class; refusing to bind another credential", "org", orgID)
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: unknownGitHubClassMessage})
			return true
		}
		internalError(w, "github-access", err)
		return true
	}
	if !inTheWay {
		return false
	}
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: managedInTheWayMessage})
	return true
}

// handleGitHubManagedDisconnect moves the workspace off the deployment App:
// every live installation row is soft-removed and the class returns to the
// rowless default. Org admin. A verb route because it is a multi-row
// transition serialized under the App-registration lock, not a field write —
// disconnectManagedInstallations says exactly what "one motion" means on each
// dialect.
//
// Idempotent: a workspace with nothing bound is a 204 with nothing written —
// the workspace-addressed verb always finds its workspace, so there is no
// resource to 404 about.
//
// POST /api/orgs/{org_id}/github/managed/disconnect
func (s *Server) handleGitHubManagedDisconnect(w http.ResponseWriter, r *http.Request) {
	s.serveManagedDisconnect(w, r, "")
}

// handleGitHubManagedInstallationDisconnect is the same verb narrowed to one
// installation: a workspace holds one per GitHub account and may want to drop
// one account without leaving the class. Dropping the last one IS the full
// disconnect — the class resets — so the two verbs cannot leave the rows and
// the class disagreeing.
//
// The row is the resource here, so an installation this workspace does not
// hold live is a 404 — the same answer as for one it never held, because it is
// in neither case in the workspace's own list, and another workspace's row is
// nobody's business.
//
// POST /api/orgs/{org_id}/github/managed/installations/{installation_id}/disconnect
func (s *Server) handleGitHubManagedInstallationDisconnect(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.PathValue("installation_id"))
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonInvalidField,
			Message: "installation_id must be a positive integer", Field: "installation_id"})
		return
	}
	s.serveManagedDisconnect(w, r, strconv.FormatInt(id, 10))
}

// serveManagedDisconnect is both verbs: the whole workspace when only is "",
// one installation otherwise.
func (s *Server) serveManagedDisconnect(w http.ResponseWriter, r *http.Request, only string) {
	if s.deployCfg == nil || runmode.Current() != runmode.ModeMulti {
		// Same door as the Connect click: the deployment App is a multi-mode
		// credential, so in local mode the class is unreachable and the verb
		// that leaves it does not exist.
		notFound(w, "route")
		return
	}
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	// The App-registration lock, keyed by org — the same one every credential
	// transition takes. A PAT bind or a BYO registration racing this verb lands
	// wholly before or wholly after it, and whichever runs second reads the
	// other's committed state: a bind that arrives second meets the guard on an
	// empty workspace and proceeds, or meets a live row and refuses.
	release, err := s.acquireKeyedLock(ctx, &s.githubAppRegMu, githubAppRegRMWLockSalt, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}
	defer release()

	// The class decides whose rows these are. A BYO workspace's installation
	// rows belong to its own App and are torn down by its own switch flow; this
	// verb must never touch them, so it refuses rather than no-ops — a 204 here
	// would tell an admin their workspace left a class it was never in.
	class, err := s.githubCredentialClass(ctx, orgID)
	if err != nil {
		if errors.Is(err, ErrUnknownGitHubCredentialClass) {
			githubAppLog.Error("unknown github credential class; refusing managed disconnect", "org", orgID)
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: unknownGitHubClassMessage})
			return
		}
		internalError(w, "github-access", err)
		return
	}
	if class == domain.GitHubCredentialClassBYOApp {
		httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict,
			Message: "this workspace uses its own GitHub App — use the switch flow"})
		return
	}

	live, err := s.githubApps.ListInstallationsForOrgSystem(ctx, orgID)
	if err != nil {
		internalError(w, "github-access", err)
		return
	}
	remove := live
	if only != "" {
		remove = nil
		for _, inst := range live {
			if inst.InstallationID == only {
				remove = []domain.OrgGitHubAppInstallation{inst}
			}
		}
		if len(remove) == 0 {
			notFound(w, "installation")
			return
		}
	}
	// The class resets when the workspace's last managed row goes: on the
	// whole-workspace verb that is always, on the narrowed one only when the
	// row being dropped is the last. A workspace already on pat with rows still
	// live is the disagreement this file exists to end, so its rows go too and
	// its class simply stays where it is.
	resetClass := class == domain.GitHubCredentialClassManagedApp && len(remove) == len(live)
	if len(remove) == 0 && !resetClass {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := s.disconnectManagedInstallations(ctx, orgID, userID, remove, resetClass); err != nil {
		githubAppLog.Error("managed disconnect failed", "org", orgID, "error", err)
		internalError(w, "github-access", err)
		return
	}
	release() // idempotent; the defer stays as the early-return safety net

	for _, inst := range remove {
		githubAppLog.Info("disconnected managed installation",
			"org", orgID, "installation", inst.InstallationID, "account", inst.AccountLogin, "host", inst.GitHubHost)
		// A token minted from the row before it was removed would otherwise
		// keep working for up to an hour, which is exactly what an
		// installation.deleted delivery also cuts short.
		s.invalidateInstallationToken(orgID, inst.InstallationID)
	}
	if len(remove) > 0 {
		// The credential the pollers ran under is gone. Re-due them under
		// whatever remains, as every other credential transition does.
		s.kickGitHubChanged(r, orgID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// disconnectManagedInstallations is the verb's write: the soft-removes, the
// class reset, and one access-change row per account disconnected, recorded
// beside the bind that connected it and named the same way.
//
// The rows go first, then the class. That order is what makes a failure
// between the two recoverable in the one place the two writes cannot share a
// transaction: on Postgres the installation table admits only admin-pool
// writes, so each soft-remove commits on its own while the class and the audit
// rows commit with the request's transaction. Rows-then-class means the worst
// partial outcome is a managed class over no live rows — a state that resolves
// nothing, routes nothing, and is exactly what a repeat of this verb converges,
// since it resets the class of a managed workspace with no rows. The other
// order could leave a pat class over live rows, which is the hole this verb
// exists to close.
func (s *Server) disconnectManagedInstallations(ctx context.Context, orgID, userID string, remove []domain.OrgGitHubAppInstallation, resetClass bool) error {
	return s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		for _, inst := range remove {
			// The soft-remove is what an installation.deleted delivery does,
			// cascade included: the reachable-repo rows and the scope marker go
			// with the row, so nothing keeps vouching for a reach the
			// workspace no longer has.
			if _, err := tx.GitHubApps.MarkInstallationRemoved(ctx, orgID, inst.InstallationID); err != nil {
				return fmt.Errorf("remove installation %s: %w", inst.InstallationID, err)
			}
		}
		if resetClass {
			// pat is the rowless default: a PAT class with no stored PAT is
			// "unconfigured", the state a fresh workspace with nothing bound
			// is in.
			if _, err := tx.Orgs.SetGitHubCredentialClass(ctx, orgID, domain.GitHubCredentialClassPAT); err != nil {
				return fmt.Errorf("set github credential class: %w", err)
			}
		}
		for _, inst := range remove {
			if err := tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
				ActorUserID: userID,
				Action:      domain.AccessActionCredentialRemoved,
				DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindGitHubApp, inst.GitHubHost, inst.AccountLogin),
			}); err != nil {
				return fmt.Errorf("record access change: %w", err)
			}
		}
		return nil
	})
}
