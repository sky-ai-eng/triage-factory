package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/runident"
	"github.com/sky-ai-eng/triage-factory/internal/agentmeta"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
)

// LocalClient is the in-process implementation of Client. Holds a
// resolved RunInfo (set at construction by either AutoDetect's env
// path or by the daemon's per-socket handler) and the db.Stores
// bundle the binary is wired against. Every write branches on
// info.IsEventTriggered to choose admin-pool `...System` calls vs
// the synthetic-claims tx wrap — the same branch the pre-agenthost
// subcommand bodies inlined verbatim, now hoisted up one level.
//
// Concurrency: not safe for concurrent calls from independent
// goroutines on a single instance. cmd/exec subcommands are single-
// goroutine; the daemon constructs a fresh LocalClient per request
// (it's cheap — just two pointers) so cross-request isolation is
// trivially preserved without serializing.
type LocalClient struct {
	stores db.Stores
	info   RunInfo

	// ghResolver overrides the GitHub credential resolver. Nil in
	// production (githubResolver builds the real one from stores); set by
	// tests to inject a fake resolver covering both the App-installation and
	// PAT tiers without standing up the full App-mint plumbing.
	ghResolver ghclient.Resolver
}

// NewLocal builds a LocalClient bound to the given stores + identity.
// Callers that resolve identity from env (AutoDetect's local branch)
// hand the resolved RunInfo here; the daemon hands the per-socket
// run info here at request dispatch.
func NewLocal(stores db.Stores, info RunInfo) *LocalClient {
	return &LocalClient{stores: stores, info: info}
}

func (c *LocalClient) LookupRun(_ context.Context) (RunInfo, error) {
	// Empty RunID at this stage means AutoDetect's env probe was
	// bypassed (test seam) or the caller constructed a LocalClient
	// directly with a zero-value RunInfo. Surface the same sentinel
	// the runident path does so subcommand helpers can translate it
	// to their package-local "missing run id" sentinel without
	// having to distinguish callers.
	if c.info.RunID == "" {
		return RunInfo{}, runident.ErrRunIdentityMissing
	}
	return c.info, nil
}

func (c *LocalClient) Close() error { return nil }

// withWrite picks the per-run routing strategy: event-triggered runs
// route the write through the admin-pool `...System` variant of the
// store call (no JWT claims available — the trigger fired in a
// background goroutine); manual runs wrap the call in
// SyntheticClaimsWithTx with the kicking-off human's identity so RLS
// policies see the right (orgID, userID) pair. The two-arg shape lets
// each call site pick its own admin-pool function and tx-pool closure
// without duplicating the if/else everywhere.
func (c *LocalClient) withWrite(
	ctx context.Context,
	system func() error,
	user func(ts db.TxStores) error,
) error {
	if c.info.IsEventTriggered {
		return system()
	}
	return c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, user)
}

// --- pending reviews ---

func (c *LocalClient) GetPendingReview(ctx context.Context, reviewID string) (*domain.PendingReview, error) {
	return c.stores.Reviews.GetSystem(ctx, c.info.OrgID, reviewID)
}

func (c *LocalClient) CreatePendingReview(ctx context.Context, r domain.PendingReview) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.CreateSystem(ctx, c.info.OrgID, r) },
		func(ts db.TxStores) error { return ts.Reviews.Create(ctx, c.info.OrgID, r) },
	)
}

func (c *LocalClient) DeletePendingReview(ctx context.Context, reviewID string) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.DeleteSystem(ctx, c.info.OrgID, reviewID) },
		func(ts db.TxStores) error { return ts.Reviews.Delete(ctx, c.info.OrgID, reviewID) },
	)
}

func (c *LocalClient) LockReviewSubmission(ctx context.Context, reviewID, body, event string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.Reviews.LockSubmissionSystem(ctx, c.info.OrgID, reviewID, body, event)
		},
		func(ts db.TxStores) error {
			return ts.Reviews.LockSubmission(ctx, c.info.OrgID, reviewID, body, event)
		},
	)
}

func (c *LocalClient) AddPendingReviewComment(ctx context.Context, comment domain.PendingReviewComment) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.AddCommentSystem(ctx, c.info.OrgID, comment) },
		func(ts db.TxStores) error { return ts.Reviews.AddComment(ctx, c.info.OrgID, comment) },
	)
}

func (c *LocalClient) UpdatePendingReviewComment(ctx context.Context, commentID, body string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.Reviews.UpdateCommentSystem(ctx, c.info.OrgID, commentID, body)
		},
		func(ts db.TxStores) error {
			return ts.Reviews.UpdateComment(ctx, c.info.OrgID, commentID, body)
		},
	)
}

func (c *LocalClient) DeletePendingReviewComment(ctx context.Context, commentID string) error {
	return c.withWrite(ctx,
		func() error { return c.stores.Reviews.DeleteCommentSystem(ctx, c.info.OrgID, commentID) },
		func(ts db.TxStores) error { return ts.Reviews.DeleteComment(ctx, c.info.OrgID, commentID) },
	)
}

func (c *LocalClient) ListPendingReviewComments(ctx context.Context, reviewID string) ([]domain.PendingReviewComment, error) {
	return c.stores.Reviews.ListCommentsSystem(ctx, c.info.OrgID, reviewID)
}

// --- pending PRs ---

func (c *LocalClient) GetPendingPRByRunID(ctx context.Context) (*domain.PendingPR, error) {
	return c.stores.PendingPRs.ByRunIDSystem(ctx, c.info.OrgID, c.info.RunID)
}

// CreateAndLockPendingPR collapses the old Create + Lock pair into a
// single agenthost call. The manual path runs both inside one
// synthetic-claims tx (atomic — a crash between Create and Lock used
// to strand an unlocked row, see the TODO removed in this refactor).
// The event-triggered path still does them as two admin-pool calls
// because there's no shared tx surface across the admin pool's
// statements; the second-layer Lock is still load-bearing for the
// rare insert-but-no-lock race two concurrent `pr create` invocations
// can produce.
func (c *LocalClient) CreateAndLockPendingPR(ctx context.Context, row domain.PendingPR) error {
	return c.withWrite(ctx,
		func() error {
			if err := c.stores.PendingPRs.CreateSystem(ctx, c.info.OrgID, row); err != nil {
				return err
			}
			return c.stores.PendingPRs.LockSystem(ctx, c.info.OrgID, row.ID, row.Title, row.Body)
		},
		func(ts db.TxStores) error {
			if err := ts.PendingPRs.Create(ctx, c.info.OrgID, row); err != nil {
				return err
			}
			return ts.PendingPRs.Lock(ctx, c.info.OrgID, row.ID, row.Title, row.Body)
		},
	)
}

func (c *LocalClient) LockPendingPR(ctx context.Context, id, title, body string) error {
	return c.withWrite(ctx,
		func() error { return c.stores.PendingPRs.LockSystem(ctx, c.info.OrgID, id, title, body) },
		func(ts db.TxStores) error { return ts.PendingPRs.Lock(ctx, c.info.OrgID, id, title, body) },
	)
}

// --- workspace ---

func (c *LocalClient) GetAgentRun(ctx context.Context) (*domain.AgentRun, error) {
	return c.stores.AgentRuns.GetSystem(ctx, c.info.OrgID, c.info.RunID)
}

func (c *LocalClient) GetTask(ctx context.Context, taskID string) (*domain.Task, error) {
	return c.stores.Tasks.GetSystem(ctx, c.info.OrgID, taskID)
}

func (c *LocalClient) ListRepos(ctx context.Context) ([]domain.RepoProfile, error) {
	return c.stores.Repos.ListSystem(ctx, c.info.OrgID)
}

func (c *LocalClient) GetRepo(ctx context.Context, repoID string) (*domain.RepoProfile, error) {
	return c.stores.Repos.GetSystem(ctx, c.info.OrgID, repoID)
}

func (c *LocalClient) GetRunWorktreeByRepo(ctx context.Context, repoID string) (*domain.RunWorktree, error) {
	return c.stores.RunWorktrees.GetByRepoSystem(ctx, c.info.OrgID, c.info.RunID, repoID)
}

func (c *LocalClient) ListRunWorktrees(ctx context.Context) ([]domain.RunWorktree, error) {
	return c.stores.RunWorktrees.ListSystem(ctx, c.info.OrgID, c.info.RunID)
}

func (c *LocalClient) InsertRunWorktree(ctx context.Context, row domain.RunWorktree) (bool, string, error) {
	if c.info.IsEventTriggered {
		return c.stores.RunWorktrees.InsertSystem(ctx, c.info.OrgID, row)
	}
	var (
		inserted    bool
		winningPath string
	)
	err := c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, func(ts db.TxStores) error {
		i, w, ierr := ts.RunWorktrees.Insert(ctx, c.info.OrgID, row)
		inserted = i
		winningPath = w
		return ierr
	})
	return inserted, winningPath, err
}

func (c *LocalClient) DeleteRunWorktreeByRepo(ctx context.Context, repoID string) error {
	return c.withWrite(ctx,
		func() error {
			return c.stores.RunWorktrees.DeleteByRepoSystem(ctx, c.info.OrgID, c.info.RunID, repoID)
		},
		func(ts db.TxStores) error {
			return ts.RunWorktrees.DeleteByRepo(ctx, c.info.OrgID, c.info.RunID, repoID)
		},
	)
}

// --- agent run footer ---

func (c *LocalClient) BuildAgentRunFooter(_ context.Context, kind string) (string, error) {
	return agentmeta.Build(c.stores.AgentRuns, c.info.OrgID, c.info.RunID, kind), nil
}

// --- artifacts ---

// UpsertArtifact stamps the run identity (run_id, org_id, team_id) onto the
// caller's polymorphic artifact and upserts it. Event-triggered runs route
// admin-pool (no JWT claims to bind); manual runs wrap the app-pool write in
// the kicking-off user's synthetic claims — the same branch the pending-PR
// writer uses. Returns the stored row. See TFAC-460.
func (c *LocalClient) UpsertArtifact(ctx context.Context, a domain.Artifact) (domain.Artifact, error) {
	a.OrgID = c.info.OrgID
	a.TeamID = c.info.TeamID
	a.RunID = c.info.RunID
	if c.info.IsEventTriggered {
		return c.stores.Artifacts.UpsertSystem(ctx, c.info.OrgID, a)
	}
	var out domain.Artifact
	err := c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, func(ts db.TxStores) error {
		stored, uerr := ts.Artifacts.Upsert(ctx, c.info.OrgID, a)
		out = stored
		return uerr
	})
	return out, err
}

// --- jira ---
//
// jiraSystemClient builds the org's bot-attributed Jira client via the
// ForSystem resolver. The secret store backing it differs by mode but
// the call site doesn't care: local mode reads the OS keychain on the
// user's own machine; the daemon (sandbox path) reads the host's
// Vault-backed store — the host can read the credential the sandboxed
// agent can't. Either way the agent process never holds the token.
//
// A missing credential maps to a clear "not configured" message (the
// guidance exec printed before handing off); every other resolver error
// (a transient vault/keychain outage) propagates so it isn't misreported
// as "absent". Over IPC the message crosses as the response Error string,
// so the agent reads the identical text in both modes.
func (c *LocalClient) jiraSystemClient(ctx context.Context) (*jiraclient.Client, error) {
	client, err := jiraclient.NewResolver(c.stores.Secrets, c.stores.Orgs).ForSystem(ctx, c.info.OrgID)
	if errors.Is(err, jiraclient.ErrNoJiraSystemCredential) {
		return nil, errors.New("no Jira credential configured; run triagefactory and complete setup first")
	}
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (c *LocalClient) JiraGetIssue(ctx context.Context, key string) (*jiraclient.Issue, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetIssue(ctx, key)
}

func (c *LocalClient) JiraTransitionTo(ctx context.Context, key, status string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.TransitionTo(ctx, key, status); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ArtifactStateIssueUpdated, jiraDetailsJSON(map[string]any{"status": status}))
	return nil
}

func (c *LocalClient) JiraGetTransitions(ctx context.Context, key string) ([]jiraclient.Transition, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetTransitions(ctx, key)
}

func (c *LocalClient) JiraAddComment(ctx context.Context, key, body string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	commentID, err := client.AddComment(ctx, key, body)
	if err != nil {
		return err
	}
	c.recordJiraComment(ctx, key, commentID, body)
	return nil
}

func (c *LocalClient) JiraAssignToSelf(ctx context.Context, key string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.AssignToSelf(ctx, key); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ArtifactStateIssueUpdated, jiraDetailsJSON(map[string]any{"assignee": "self"}))
	return nil
}

func (c *LocalClient) JiraUnassign(ctx context.Context, key string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.Unassign(ctx, key); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ArtifactStateIssueUpdated, jiraDetailsJSON(map[string]any{"assignee": nil}))
	return nil
}

func (c *LocalClient) JiraCreateIssue(ctx context.Context, project, issueType, summary, description, parentKey, priority string) (string, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return "", err
	}
	key, err := client.CreateIssue(ctx, project, issueType, summary, description, parentKey, priority)
	if err != nil {
		return "", err
	}
	c.recordJiraIssue(ctx, key, domain.ArtifactStateIssueCreated, "")
	return key, nil
}

func (c *LocalClient) JiraUpdateIssue(ctx context.Context, key string, fields jiraclient.UpdateIssueFields) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.UpdateIssue(ctx, key, fields); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ArtifactStateIssueUpdated, jiraDetailsJSON(map[string]any{"fields": updatedFieldNames(fields)}))
	return nil
}

func (c *LocalClient) JiraSetParent(ctx context.Context, key, parentKey string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.SetParent(ctx, key, parentKey); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ArtifactStateIssueUpdated, jiraDetailsJSON(map[string]any{"parent": parentKey}))
	return nil
}

func (c *LocalClient) JiraGetChildIssues(ctx context.Context, key string) ([]jiraclient.Issue, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetChildIssues(ctx, key)
}

func (c *LocalClient) JiraSearchIssues(ctx context.Context, jql string, fields []string, maxResults int) ([]jiraclient.Issue, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.SearchIssues(ctx, jql, fields, maxResults)
}

func (c *LocalClient) JiraListPriorities(ctx context.Context) ([]jiraclient.Priority, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListPriorities(ctx)
}

func (c *LocalClient) JiraSetPriority(ctx context.Context, key, priority string) error {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return err
	}
	if err := client.SetPriority(ctx, key, priority); err != nil {
		return err
	}
	c.recordJiraIssue(ctx, key, domain.ArtifactStateIssueUpdated, jiraDetailsJSON(map[string]any{"priority": priority}))
	return nil
}

func (c *LocalClient) JiraListIssueTypes(ctx context.Context, project string) ([]jiraclient.IssueType, error) {
	client, err := c.jiraSystemClient(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListIssueTypes(ctx, project)
}

// --- jira artifact recording (TFAC-459) ---
//
// Record one durable `artifacts` row per successful Jira mutation the agent
// performs, attributed to this run. Jira writes are not preview-gated — they
// execute immediately — so this is straight record-on-success: the call above
// already took effect, and recording is best-effort, never able to fail or
// roll back the action it observed.
//
// Recording happens here on the LocalClient because the multi daemon
// dispatches every host-routed Jira call through NewLocal (server.go), so this
// one seam covers both the sandbox (daemon) and the local-mode CLI, with the
// full RunInfo + Stores in scope.
//
// Every mediated Jira *mutation* is captured: issue create, and the in-place
// edits — transition, assign/unassign, update-fields, set-parent, set-priority
// — plus comments. Pure reads (get / search / list-*) record nothing. This is
// the full write surface of `exec jira`, so an agent can't mutate an issue
// without leaving an audit trail.

// recordJiraIssue upserts the single deduped `issue` artifact for KEY. Every
// issue mutation (create / transition / assign / unassign / update / set-parent
// / set-priority) collapses onto one row keyed jira:issue:<KEY>; external_id
// and url are the issue's stable coordinates, populated on every action so a
// later one can fill a URL an earlier one couldn't compute (the store preserves
// them on empty either way — see ArtifactStore.Upsert). state + details_json
// carry the most recent action — by design the row tracks the last action, not
// the first (domain.Artifact).
func (c *LocalClient) recordJiraIssue(ctx context.Context, key, state, detailsJSON string) {
	if c.stores.Artifacts == nil || key == "" {
		return
	}
	c.upsertJiraArtifact(ctx, domain.Artifact{
		Kind:        domain.ArtifactKindIssue,
		Target:      key,
		ExternalID:  key,
		URL:         c.jiraBrowseURL(ctx, key),
		State:       state,
		DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindIssue, key, ""),
		DetailsJSON: detailsJSON,
	})
}

// recordJiraComment upserts a `comment` artifact keyed jira:comment:<id>. A
// missing comment id (the POST landed but its body didn't parse) means there's
// no stable key to dedup on, so recording is skipped — the comment still
// posted; only its audit row is best-effort dropped. The skip is logged at
// debug so a future Jira response-shape change surfaces as missing rows with a
// breadcrumb, not silently.
func (c *LocalClient) recordJiraComment(ctx context.Context, key, commentID, body string) {
	if c.stores.Artifacts == nil {
		return
	}
	if commentID == "" {
		agenthostLog.Debug("jira comment recorded without an id; skipping artifact",
			"run", c.info.RunID, "issue", key)
		return
	}
	c.upsertJiraArtifact(ctx, domain.Artifact{
		Kind:        domain.ArtifactKindComment,
		Target:      key,
		ExternalID:  commentID,
		URL:         c.jiraCommentURL(ctx, key, commentID),
		State:       domain.ArtifactStateCommentPosted,
		DedupKey:    domain.ArtifactDedupKey(domain.ArtifactProviderJira, domain.ArtifactKindComment, commentID, ""),
		DetailsJSON: jiraDetailsJSON(map[string]any{"body": jiraBodySnippet(body)}),
	})
}

// upsertJiraArtifact stamps the run attribution (run/org/team) + provider onto
// a and writes it best-effort: a recording failure is logged and swallowed so
// it never fails the agent's already-applied Jira action. The write routes the
// same way every other exec choke-point write does (withWrite) — admin pool
// for event-triggered runs (no user), a synthetic-claims tx for manual ones.
func (c *LocalClient) upsertJiraArtifact(ctx context.Context, a domain.Artifact) {
	if c.stores.Artifacts == nil {
		return
	}
	a.RunID = c.info.RunID
	a.OrgID = c.info.OrgID
	a.TeamID = c.info.TeamID
	a.Provider = domain.ArtifactProviderJira

	err := c.withWrite(ctx,
		func() error {
			_, e := c.stores.Artifacts.UpsertSystem(ctx, c.info.OrgID, a)
			return e
		},
		func(ts db.TxStores) error {
			_, e := ts.Artifacts.Upsert(ctx, c.info.OrgID, a)
			return e
		},
	)
	if err != nil {
		agenthostLog.Warn("jira artifact recording failed (action already applied)",
			"run", c.info.RunID, "kind", a.Kind, "target", a.Target, "error", err)
	}
}

// jiraSiteBase returns the org's configured Jira site URL, trailing slash
// trimmed, or "" if it's unreadable or unset. Best-effort and uses the
// admin-pool settings read so it works without a JWT-claims context, like the
// rest of recording. This is the human-facing site (e.g.
// https://acme.atlassian.net), not the API gateway base the Jira client talks
// to — the same source the poller stamps entity URLs from.
func (c *LocalClient) jiraSiteBase(ctx context.Context) string {
	if c.stores.Orgs == nil {
		return ""
	}
	set, err := c.stores.Orgs.GetSettingsSystem(ctx, c.info.OrgID)
	if err != nil || set.JiraBaseURL == "" {
		return ""
	}
	return strings.TrimRight(set.JiraBaseURL, "/")
}

// jiraBrowseURL builds the issue's human-facing {site}/browse/<KEY> link, or ""
// when the site URL is unavailable (url is an optional artifact field).
func (c *LocalClient) jiraBrowseURL(ctx context.Context, key string) string {
	base := c.jiraSiteBase(ctx)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/browse/%s", base, key)
}

// jiraCommentURL builds a deep link to the comment —
// {site}/browse/<KEY>?focusedCommentId=<id> — which both Cloud and Server/DC
// honor. Empty when the site URL is unavailable.
func (c *LocalClient) jiraCommentURL(ctx context.Context, key, commentID string) string {
	base := c.jiraSiteBase(ctx)
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/browse/%s?focusedCommentId=%s", base, key, commentID)
}

// jiraDetailsJSON marshals a small kind-specific payload for an artifact's
// details_json. Returns "" (→ SQL NULL) on an empty map or a marshal error —
// details are descriptive metadata, never load-bearing, so a failure here must
// not break recording.
func jiraDetailsJSON(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

// updatedFieldNames lists which issue fields an update touched, for the
// artifact's details_json — the names only, never the (potentially large)
// values. Order is stable so the recorded payload is deterministic.
func updatedFieldNames(f jiraclient.UpdateIssueFields) []string {
	names := []string{}
	if f.Summary != nil {
		names = append(names, "summary")
	}
	if f.Description != nil {
		names = append(names, "description")
	}
	if f.Priority != nil {
		names = append(names, "priority")
	}
	if f.IssueType != nil {
		names = append(names, "issue_type")
	}
	if len(f.AddLabels) > 0 {
		names = append(names, "add_labels")
	}
	if len(f.RemoveLabels) > 0 {
		names = append(names, "remove_labels")
	}
	return names
}

// jiraBodySnippet trims a comment body to a short, stored-once reference for
// the artifact's details_json. Rune-aware so a multibyte char isn't split.
func jiraBodySnippet(body string) string {
	const max = 200
	body = strings.TrimSpace(body)
	r := []rune(body)
	if len(r) <= max {
		return body
	}
	return string(r[:max]) + "…"
}

// --- github ---
//
// githubResolver returns the credential resolver to use. Tests inject one
// via the ghResolver field; production builds the real org-tiered resolver
// (App installation token → org PAT) from this client's stores. The secret
// store backing it differs by mode but the call site doesn't care: local
// mode reads the OS keychain on the user's own machine; the daemon (sandbox
// path) reads the host's Vault-backed store — the host can read the
// credential the sandboxed agent can't. Either way the agent process never
// holds a token.
//
// The resolver is memoized on first build so several GitHub calls within one
// exec subcommand (e.g. pr start-review's GetPR + GetPRDiff + GetPRFiles)
// share one in-memory installation-token cache instead of re-minting per
// call. Safe because LocalClient is single-goroutine (see the type doc). The
// daemon pre-sets ghResolver to the Server's shared resolver, so this build
// path is the local-mode CLI's only. App-or-PAT tier selection lives entirely
// in the resolver — no gh-specific credential logic here.
func (c *LocalClient) githubResolver() ghclient.Resolver {
	if c.ghResolver == nil {
		c.ghResolver = ghclient.NewResolver(c.stores.Secrets, c.stores.GitHubApps, c.stores.Orgs, c.stores.Agents, nil)
	}
	return c.ghResolver
}

// mapGithubResolveErr maps a resolver error to the agent-facing form: a
// missing credential becomes the same "not configured" guidance the gh branch
// printed before this refactor; every other resolver error (a transient
// vault/keychain outage) propagates verbatim so it isn't misreported as
// "absent". Over IPC the message crosses as the response Error string.
func mapGithubResolveErr(err error) error {
	if errors.Is(err, ghclient.ErrNoGitHubCredentials) {
		return errors.New("GitHub not configured; run triagefactory and complete setup first")
	}
	return err
}

// githubClientForRepo resolves an authenticated client for owner/repo.
func (c *LocalClient) githubClientForRepo(ctx context.Context, owner, repo string) (*ghclient.Client, error) {
	client, err := c.githubResolver().ClientForRepo(ctx, c.info.OrgID, owner, repo)
	if err != nil {
		return nil, mapGithubResolveErr(err)
	}
	return client, nil
}

// githubClientAndIdentityForRepo resolves the client AND the acting credential's
// identity (App/service-account vs real-user PAT) in one pass, for the
// pending-review collision check. The identity is knowable only here, where the
// token resolves. When the resolver implements ghclient.RepoIdentityResolver
// (the production resolver does), the two come from a single resolution so they
// describe the same credential; otherwise it falls back to ClientForRepo and
// reports IdentityUnknown, which the collision check treats conservatively
// (never reuses, so it can't hijack a review it can't prove is the bot's own).
func (c *LocalClient) githubClientAndIdentityForRepo(ctx context.Context, owner, repo string) (*ghclient.Client, ghclient.Identity, error) {
	if ir, ok := c.githubResolver().(ghclient.RepoIdentityResolver); ok {
		client, identity, err := ir.ClientForRepoWithIdentity(ctx, c.info.OrgID, owner, repo)
		if err != nil {
			return nil, ghclient.IdentityUnknown, mapGithubResolveErr(err)
		}
		return client, identity, nil
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	return client, ghclient.IdentityUnknown, err
}

func (c *LocalClient) GithubGetPR(ctx context.Context, owner, repo string, number int, verbose bool) (*ghclient.PRView, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetPR(owner, repo, number, verbose)
}

func (c *LocalClient) GithubGetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return client.GetPRDiff(owner, repo, number, file)
}

func (c *LocalClient) GithubGetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetPRFiles(owner, repo, number)
}

func (c *LocalClient) GithubGetCommentThread(ctx context.Context, owner, repo string, commentID, page int) (*ghclient.CommentThread, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetCommentThread(owner, repo, commentID, page)
}

func (c *LocalClient) GithubGetReviewDetail(ctx context.Context, owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetReviewDetail(owner, repo, number, reviewID, verbose)
}

func (c *LocalClient) GithubDismissReview(ctx context.Context, owner, repo string, number, reviewID int, message string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.DismissReview(owner, repo, number, reviewID, message)
}

func (c *LocalClient) GithubSubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", err
	}
	return client.SubmitReview(owner, repo, number, commitSHA, event, body, comments)
}

func (c *LocalClient) GithubCreatePR(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (int, string, string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", "", err
	}
	return client.CreatePR(owner, repo, head, base, title, body, draft)
}

// GithubCreatePendingReview resolves the client and acting identity, then folds
// in the start-review collision check before creating. If a pending review
// already exists for the acting identity on this PR: an App/service-account
// identity reuses it (the review is the bot's own from a prior run) and returns
// its id; a real-user-PAT (or unknown) identity returns ErrPendingReviewCollision
// rather than risk modifying a human's in-progress review. Only when no pending
// review exists does it create a fresh one, seeding it with commitSHA/comments.
//
// Reuse (return the existing id) is chosen over clear-and-recreate: it's
// non-destructive and lets the caller keep appending to the bot's own review.
func (c *LocalClient) GithubCreatePendingReview(ctx context.Context, owner, repo string, number int, commitSHA string, comments []ghclient.SubmitReviewComment) (string, error) {
	client, identity, err := c.githubClientAndIdentityForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	existingID, _, err := client.GetPendingReview(owner, repo, number)
	if err != nil {
		return "", err
	}
	if existingID != "" {
		if identity == ghclient.IdentityApp {
			return existingID, nil // the bot's own review from a prior run — reuse it
		}
		// Real-user PAT (or an identity we couldn't determine): the existing
		// review might be a human's live work. Never hijack it.
		return "", ErrPendingReviewCollision
	}

	return client.CreatePendingReview(owner, repo, number, commitSHA, comments)
}

// GithubAddPendingReviewComment adds one inline comment to an existing pending
// review (addressed by its GraphQL node id). owner/repo resolve the per-repo
// credential host-side; the github.Client op keys solely off the review node
// id. line>0 with a non-nil startLine makes it a multi-line range.
func (c *LocalClient) GithubAddPendingReviewComment(ctx context.Context, owner, repo, reviewID, path, body string, line int, startLine *int) (string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return client.AddPendingReviewComment(reviewID, ghclient.SubmitReviewComment{
		Path:      path,
		Line:      line,
		StartLine: startLine,
		Body:      body,
	})
}

// GithubGetPendingReview returns the acting identity's current pending review on
// the PR (its node id plus inline comments), or ("", nil, nil) when there is
// none. It backs both the start-review collision read and a plain editor sync.
func (c *LocalClient) GithubGetPendingReview(ctx context.Context, owner, repo string, number int) (string, []ghclient.PendingReviewComment, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", nil, err
	}
	return client.GetPendingReview(owner, repo, number)
}

func (c *LocalClient) GithubAddComment(ctx context.Context, owner, repo string, number int, body string) (int, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return client.AddComment(owner, repo, number, body)
}

func (c *LocalClient) GithubReplyToComment(ctx context.Context, owner, repo string, number, commentID int, body string) (int, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return client.ReplyToComment(owner, repo, number, commentID, body)
}

func (c *LocalClient) GithubReactToComment(ctx context.Context, owner, repo string, commentID int, emoji string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.ReactToComment(owner, repo, commentID, emoji)
}

func (c *LocalClient) GithubUpdateComment(ctx context.Context, owner, repo string, commentID int, body string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.UpdateComment(owner, repo, commentID, body)
}

func (c *LocalClient) GithubDeleteComment(ctx context.Context, owner, repo string, commentID int) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.DeleteComment(owner, repo, commentID)
}

func (c *LocalClient) GithubAPIGet(ctx context.Context, owner, repo, path string) ([]byte, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.Get(path)
}

func (c *LocalClient) GithubDownloadArtifact(ctx context.Context, owner, repo, path string, dst io.Writer, maxBytes int64) (int64, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return client.DownloadArtifact(ctx, path, dst, maxBytes)
}
