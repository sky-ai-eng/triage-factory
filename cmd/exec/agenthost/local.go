package agenthost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
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

// --- review draft finalization ---

// FinalizeReviewDraft is the host side of `gh pr submit-review` under the
// GitHub-native model: it does NOT submit to GitHub (approval does that). It
// locates the run's pending review artifact, reads the live GitHub pending
// review for the inline comments the agent added, stages the body + event, and
// sets the ready sentinel (details_json.review_event) that parks the run for
// human approval — snapshotting the agent's first draft into details.proposed so
// the approve-time human-feedback diff has a baseline.
//
// reviewID is the review node id the agent passes through; it's validated
// against the artifact's ExternalID so a stale id fails loudly rather than
// finalizing the wrong review. The TFAC-358 anti-double-submit guard fires here:
// a ready sentinel that's already set returns ErrReviewAlreadyFinalized.
func (c *LocalClient) FinalizeReviewDraft(ctx context.Context, reviewID, event, body string) error {
	arts, err := c.listArtifactsByRun(ctx)
	if err != nil {
		return err
	}
	art := domain.FirstPendingReviewArtifact(arts)
	if art == nil {
		return fmt.Errorf("no pending review for this run — run `gh pr start-review` first")
	}
	if reviewID != "" && art.ExternalID != reviewID {
		return fmt.Errorf("review %s does not match this run's pending review %s", reviewID, art.ExternalID)
	}

	details, err := domain.ParseReviewArtifactDetails(art.DetailsJSON)
	if err != nil {
		return fmt.Errorf("parse review artifact details: %w", err)
	}
	// Anti-double-submit (TFAC-358): the ready sentinel is already set, so the
	// agent already finalized this review. Hard error so it stops looping.
	if details.ReviewEvent != "" {
		return ErrReviewAlreadyFinalized
	}

	owner, repo, number, ok := domain.ParsePRTarget(art.Target)
	if !ok {
		return fmt.Errorf("review artifact has a malformed target %q", art.Target)
	}
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	// Read the live pending review for the inline comments the agent staged on
	// GitHub (each add-review-comment baked the severity badge into the body).
	liveID, liveComments, err := client.GetPendingReview(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("read pending review from github: %w", err)
	}
	// Guard against the recorded review drifting from the live one: if the bot's
	// pending review was deleted and recreated out-of-band between start-review and
	// now, GetPendingReview returns a different review's comments — which would be
	// snapshotted into details.Proposed as if they were the agent's. Bail rather
	// than record a draft built from someone else's review. (This is the only place
	// art.ExternalID isn't validated against the live state.)
	if liveID != "" && liveID != art.ExternalID {
		return fmt.Errorf("the pending review on GitHub (%s) no longer matches this run's recorded review (%s); it was replaced out-of-band — start a fresh review", liveID, art.ExternalID)
	}

	// A comment / request_changes review must carry something actionable: a body,
	// inline comments, or both. An approve needs neither (the approval is the
	// signal). Mirrors the old submit-review guard, now that the comments live on
	// GitHub rather than a local table.
	if body == "" && event != "APPROVE" && len(liveComments) == 0 {
		return fmt.Errorf("a %s review needs --body/--body-file or at least one inline comment", strings.ToLower(event))
	}

	proposed := domain.ReviewArtifactProposed{Body: body, Event: event}
	for _, cm := range liveComments {
		proposed.Comments = append(proposed.Comments, domain.ReviewArtifactComment{
			ID:        cm.ID,
			Path:      cm.Path,
			Line:      cm.Line,
			StartLine: cm.StartLine,
			Body:      cm.Body,
		})
	}
	details.ReviewBody = body
	details.ReviewEvent = event // the ready sentinel
	details.Proposed = proposed

	updated := *art
	updated.DetailsJSON = domain.MarshalReviewArtifactDetails(details)
	if _, err := c.UpsertArtifact(ctx, updated); err != nil {
		return fmt.Errorf("snapshot review draft into artifact: %w", err)
	}
	return nil
}

// listArtifactsByRun reads the calling run's artifacts respecting the pool split:
// event-triggered runs read admin-pool (no JWT claims); manual runs read under
// the kicking-off user's synthetic claims. Mirrors withWrite for the read side.
func (c *LocalClient) listArtifactsByRun(ctx context.Context) ([]domain.Artifact, error) {
	if c.info.IsEventTriggered {
		return c.stores.Artifacts.ListByRunSystem(ctx, c.info.OrgID, c.info.RunID)
	}
	var out []domain.Artifact
	err := c.stores.Tx.SyntheticClaimsWithTx(ctx, c.info.OrgID, c.info.UserID, func(ts db.TxStores) error {
		a, e := ts.Artifacts.ListByRun(ctx, c.info.OrgID, c.info.RunID)
		out = a
		return e
	})
	return out, err
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
	return client.GetPR(ctx, owner, repo, number, verbose)
}

func (c *LocalClient) GithubGetPRDiff(ctx context.Context, owner, repo string, number int, file string) (string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return client.GetPRDiff(ctx, owner, repo, number, file)
}

func (c *LocalClient) GithubGetPRFiles(ctx context.Context, owner, repo string, number int) ([]ghclient.PRFile, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetPRFiles(ctx, owner, repo, number)
}

func (c *LocalClient) GithubGetCommentThread(ctx context.Context, owner, repo string, commentID, page int) (*ghclient.CommentThread, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetCommentThread(ctx, owner, repo, commentID, page)
}

func (c *LocalClient) GithubGetReviewDetail(ctx context.Context, owner, repo string, number, reviewID int, verbose bool) (*ghclient.ReviewDetail, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.GetReviewDetail(ctx, owner, repo, number, reviewID, verbose)
}

func (c *LocalClient) GithubDismissReview(ctx context.Context, owner, repo string, number, reviewID int, message string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.DismissReview(ctx, owner, repo, number, reviewID, message)
}

func (c *LocalClient) GithubSubmitReview(ctx context.Context, owner, repo string, number int, commitSHA, event, body string, comments []ghclient.SubmitReviewComment) (int, string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", err
	}
	return client.SubmitReview(ctx, owner, repo, number, commitSHA, event, body, comments)
}

func (c *LocalClient) GithubCreatePR(ctx context.Context, owner, repo, head, base, title, body string, draft bool) (int, string, string, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, "", "", err
	}
	number, htmlURL, nodeID, err := client.CreatePR(ctx, owner, repo, head, base, title, body, draft)
	if err != nil {
		return 0, "", "", err
	}
	c.recordGithubPR(ctx, owner, repo, head, base, title, body, number, nodeID, htmlURL, draft)
	return number, htmlURL, nodeID, nil
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
//
// The GetPendingReview→CreatePendingReview window is not locked across runs: the
// daemon serializes one run's RPCs over its socket, but two delegated runs on
// the same PR have separate sockets and no shared lock, so both could observe
// "none" and both attempt a create. GitHub backstops this — it allows at most
// one pending review per identity per PR, so the loser's create fails with
// CreatePendingReview's mapped "one pending review" 422, never a duplicate (and
// never a hijack). The window is benign: worst case is a rare, safe error.
func (c *LocalClient) GithubCreatePendingReview(ctx context.Context, owner, repo string, number int, commitSHA string, comments []ghclient.SubmitReviewComment) (string, error) {
	client, identity, err := c.githubClientAndIdentityForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	existingID, _, err := client.GetPendingReview(ctx, owner, repo, number)
	if err != nil {
		return "", err
	}
	if existingID != "" {
		if identity == ghclient.IdentityApp {
			// The bot's own review from a prior run — reuse it, and (re)record the
			// artifact so it attributes to this run, the one now working the review.
			c.recordGithubReview(ctx, client, owner, repo, number, existingID)
			return existingID, nil
		}
		// Real-user PAT (or an identity we couldn't determine): the existing
		// review might be a human's live work. Never hijack it.
		return "", ErrPendingReviewCollision
	}

	reviewID, err := client.CreatePendingReview(ctx, owner, repo, number, commitSHA, comments)
	if err != nil {
		return "", err
	}
	c.recordGithubReview(ctx, client, owner, repo, number, reviewID)
	return reviewID, nil
}

// recordGithubReview upserts the `review` artifact for a freshly created (or
// reused) GitHub pending review, mirroring recordGithubPR. reviewID is the
// review's GraphQL node id (stored as the artifact ExternalID — the handle
// add-comment / submit / delete speak). The backing PR's own node id is fetched
// best-effort for details_json context (reconciliation keys on it, TFAC-464); a
// failure there degrades to an empty node id rather than dropping the artifact —
// nothing in the approval flow reads it, and the review row is what matters.
func (c *LocalClient) recordGithubReview(ctx context.Context, client *ghclient.Client, owner, repo string, number int, reviewID string) {
	if c.stores.Artifacts == nil {
		return
	}
	if reviewID == "" {
		agenthostLog.Debug("github pending review recorded without a review id; skipping artifact", "run", c.info.RunID, "repo", owner+"/"+repo)
		return
	}
	nodeID := ""
	if pr, perr := client.GetPRBasic(ctx, owner, repo, number); perr == nil && pr != nil {
		nodeID = pr.NodeID
	}
	c.upsertGithubArtifact(ctx, domain.NewReviewArtifact(owner+"/"+repo, number, nodeID, reviewID))
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
	return client.AddPendingReviewComment(ctx, reviewID, ghclient.SubmitReviewComment{
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
	return client.GetPendingReview(ctx, owner, repo, number)
}

func (c *LocalClient) GithubAddComment(ctx context.Context, owner, repo string, number int, body string) (int, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	id, htmlURL, err := client.AddCommentWithURL(ctx, owner, repo, number, body)
	if err != nil {
		return 0, err
	}
	c.recordGithubComment(ctx, owner, repo, number, id, htmlURL)
	return id, nil
}

func (c *LocalClient) GithubReplyToComment(ctx context.Context, owner, repo string, number, commentID int, body string) (int, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return client.ReplyToComment(ctx, owner, repo, number, commentID, body)
}

func (c *LocalClient) GithubReactToComment(ctx context.Context, owner, repo string, commentID int, emoji string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	return client.ReactToComment(ctx, owner, repo, commentID, emoji)
}

func (c *LocalClient) GithubUpdateComment(ctx context.Context, owner, repo string, commentID int, body string) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	isIssueComment, err := client.UpdateCommentScoped(ctx, owner, repo, commentID, body)
	if err != nil {
		return err
	}
	// Standalone comments only. A review line-comment — the pulls/comments
	// fallback inside UpdateCommentScoped — rides the review artifact, not a
	// comment artifact, so it records nothing here. The edit keeps the comment
	// posted; the upsert bumps the existing row (re-attributing it to this run),
	// and the store preserves the target/url an add-comment stamped, which this
	// id-only path can't recompute.
	if isIssueComment {
		c.recordGithubCommentState(ctx, commentID, domain.ArtifactStateCommentPosted)
	}
	return nil
}

func (c *LocalClient) GithubDeleteComment(ctx context.Context, owner, repo string, commentID int) error {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return err
	}
	isIssueComment, err := client.DeleteCommentScoped(ctx, owner, repo, commentID)
	if err != nil {
		return err
	}
	// Standalone comments only — a review line-comment (the pulls/comments
	// fallback) rides the review artifact. Retire the artifact in place: the row
	// persists for the audit ledger, flipped to the deleted state rather than
	// dropped.
	if isIssueComment {
		c.recordGithubCommentState(ctx, commentID, domain.ArtifactStateCommentDeleted)
	}
	return nil
}

func (c *LocalClient) GithubAPIGet(ctx context.Context, owner, repo, path string) ([]byte, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	return client.Get(ctx, path)
}

func (c *LocalClient) GithubDownloadArtifact(ctx context.Context, owner, repo, path string, dst io.Writer, maxBytes int64) (int64, error) {
	client, err := c.githubClientForRepo(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	return client.DownloadArtifact(ctx, path, dst, maxBytes)
}

// --- github artifact recording (TFAC-466) ---
//
// Record one durable `artifacts` row per successful *standalone* GitHub comment
// the agent posts via `exec gh pr` — a top-level PR/issue comment, not a review
// line-comment (those ride the review artifact, handled separately). Like the
// Jira recording above this lives on the LocalClient, the single seam the multi
// daemon dispatches every host-routed call through, so it covers both the
// sandbox (daemon) and the local-mode CLI; and like it, recording is
// best-effort and on-success-only — the comment already posted, so a store
// failure is logged and swallowed, never able to fail the action.
//
// add → comment-update → comment-delete all key on github:comment:<id>, so the
// three collapse onto one row: add stamps the full coordinates (target
// owner/repo#N, the comment id, its html_url), update bumps it in place, delete
// retires it to the deleted state. update/delete only know the comment id (not
// its PR number), so they leave target/url empty and lean on the store
// preserving them (see ArtifactStore.Upsert).

// recordGithubComment upserts the `comment` artifact for a freshly posted
// standalone comment, with every coordinate the add path knows: target
// owner/repo#N, external_id + dedup key on the comment id, and the html_url
// GitHub returned. A zero id (a 2xx whose body didn't parse) has no stable key
// to dedup on, so recording is skipped with a debug breadcrumb — the comment
// still posted.
func (c *LocalClient) recordGithubComment(ctx context.Context, owner, repo string, number, commentID int, htmlURL string) {
	if c.stores.Artifacts == nil {
		return
	}
	if commentID <= 0 {
		agenthostLog.Debug("github comment recorded without an id; skipping artifact",
			"run", c.info.RunID, "repo", owner+"/"+repo, "pr", number)
		return
	}
	c.upsertGithubArtifact(ctx, domain.Artifact{
		Kind:       domain.ArtifactKindComment,
		Target:     fmt.Sprintf("%s/%s#%d", owner, repo, number),
		ExternalID: strconv.Itoa(commentID),
		URL:        htmlURL,
		State:      domain.ArtifactStateCommentPosted,
		DedupKey:   githubCommentDedupKey(commentID),
	})
}

// recordGithubCommentState upserts the comment artifact for an edit (state
// posted) or a delete (state deleted) keyed on the same comment id. The caller
// only reaches here for a standalone (top-level issue) comment — review
// line-comments are filtered out upstream. These paths carry only the id — not
// the PR number — so they leave target/url empty; the store preserves whatever
// the add path stamped. If the comment was never recorded (e.g. the agent edits
// a human's top-level comment), the upsert mints a minimal row keyed on the id,
// which still truthfully records that this run touched it.
func (c *LocalClient) recordGithubCommentState(ctx context.Context, commentID int, state string) {
	if c.stores.Artifacts == nil {
		return
	}
	if commentID <= 0 {
		agenthostLog.Debug("github comment state change without an id; skipping artifact",
			"run", c.info.RunID, "state", state)
		return
	}
	c.upsertGithubArtifact(ctx, domain.Artifact{
		Kind:       domain.ArtifactKindComment,
		ExternalID: strconv.Itoa(commentID),
		State:      state,
		DedupKey:   githubCommentDedupKey(commentID),
	})
}

// --- github PR artifact recording ---
//
// Record the durable `pull_request` artifact when the agent opens a draft PR via
// `exec gh pr create`. Like the comment/Jira recording above this lives on the
// LocalClient — the single seam the multi daemon dispatches every host-routed
// GitHub call through (server.go's dispatch builds NewLocal) — so it covers both
// the sandbox (daemon) and the local-mode CLI. Best-effort and on-success-only:
// the PR is already open on GitHub by the time we get here, so a store failure is
// logged and swallowed, never able to fail the action. The artifact dedup key
// (github:pull_request:owner/repo#<number>) is the new idempotency guard that
// replaces the retired pending_prs one-per-run lock.

// recordGithubPR upserts the `pull_request` artifact for a freshly opened PR. The
// proposed snapshot {title, body} is the agent's draft exactly as sent to GitHub
// (no footer — that lands at human approval); node_id / head_branch / base ride
// details_json for reconciliation and the server's edit/approve handlers. A zero
// number (a 2xx whose body didn't parse) has no stable key to
// dedup on, so recording is skipped with a debug breadcrumb — the PR still
// opened. The `draft` flag selects the artifact's initial state (draft vs open);
// the new PR path always creates drafts, but recording the param keeps the row
// honest if a standalone caller ever opens a non-draft.
func (c *LocalClient) recordGithubPR(ctx context.Context, owner, repo, head, base, title, body string, number int, nodeID, htmlURL string, draft bool) {
	if c.stores.Artifacts == nil {
		return
	}
	if number <= 0 {
		agenthostLog.Debug("github PR recorded without a number; skipping artifact",
			"run", c.info.RunID, "repo", owner+"/"+repo)
		return
	}
	c.upsertGithubArtifact(ctx, domain.NewPullRequestArtifact(
		owner+"/"+repo, number, nodeID, head, base, htmlURL, title, body, draft,
	))
}

// githubCommentDedupKey is the stable key every comment action upserts on:
// github:comment:<id>.
func githubCommentDedupKey(commentID int) string {
	return domain.ArtifactDedupKey(domain.ArtifactProviderGitHub, domain.ArtifactKindComment, strconv.Itoa(commentID), "")
}

// upsertGithubArtifact stamps the run attribution (run/org/team) + provider onto
// a and writes it best-effort: a recording failure is logged and swallowed so
// it never fails the agent's already-applied GitHub action. The write routes the
// same way every other exec choke-point write does (withWrite) — admin pool for
// event-triggered runs (no user), a synthetic-claims tx for manual ones.
func (c *LocalClient) upsertGithubArtifact(ctx context.Context, a domain.Artifact) {
	if c.stores.Artifacts == nil {
		return
	}
	a.RunID = c.info.RunID
	a.OrgID = c.info.OrgID
	a.TeamID = c.info.TeamID
	a.Provider = domain.ArtifactProviderGitHub

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
		agenthostLog.Warn("github artifact recording failed (action already applied)",
			"run", c.info.RunID, "kind", a.Kind, "target", a.Target, "error", err)
	}
}
