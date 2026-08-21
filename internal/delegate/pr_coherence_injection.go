package delegate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

type prCoherenceEvent struct {
	repo       string
	prNumber   int
	headSHA    string
	oldHeadSHA string
	checkName  string
	checkURL   string
	conclusion string
}

// parsePRCoherenceEvent reads the stored event's metadata, reporting false
// unless every field this event type's note is built from is present. Event
// metadata is schema-less stored JSON, so a row is not guaranteed to carry
// what the sentence needs — and a note whose subject is blank ("CI check ""
// failed") costs the agent a turn to read and tells it nothing. Fields that
// merely qualify the note are not required here; injectionBody drops their
// clause instead.
func parsePRCoherenceEvent(evt *domain.Event) (prCoherenceEvent, bool) {
	if evt == nil {
		return prCoherenceEvent{}, false
	}
	var parsed prCoherenceEvent
	var complete bool
	switch evt.EventType {
	case domain.EventGitHubPRNewCommits:
		var meta events.GitHubPRNewCommitsMetadata
		if json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil {
			return parsed, false
		}
		parsed = prCoherenceEvent{repo: meta.Repo, prNumber: meta.PRNumber, headSHA: meta.HeadSHA, oldHeadSHA: meta.PrevHeadSHA}
		// The new head is both the news and the freshness gate's input — an
		// absent one would silently disable the gate as well as empty the copy.
		complete = parsed.headSHA != ""
	case domain.EventGitHubPRCICheckFailed:
		var meta events.GitHubPRCICheckFailedMetadata
		if json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil {
			return parsed, false
		}
		parsed = prCoherenceEvent{repo: meta.Repo, prNumber: meta.PRNumber, headSHA: meta.HeadSHA, checkName: meta.CheckName, checkURL: meta.CheckURL, conclusion: "failure"}
		complete = parsed.checkName != ""
	case domain.EventGitHubPRCICheckPassed:
		var meta events.GitHubPRCICheckPassedMetadata
		if json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil {
			return parsed, false
		}
		parsed = prCoherenceEvent{repo: meta.Repo, prNumber: meta.PRNumber, headSHA: meta.HeadSHA, checkName: meta.CheckName, conclusion: meta.Conclusion}
		complete = parsed.checkName != "" && parsed.conclusion != ""
	case domain.EventGitHubPRConflicts:
		var meta events.GitHubPRConflictsMetadata
		if json.Unmarshal([]byte(evt.MetadataJSON), &meta) != nil {
			return parsed, false
		}
		parsed = prCoherenceEvent{repo: meta.Repo, prNumber: meta.PRNumber, headSHA: meta.HeadSHA}
		// The transition into CONFLICTING is itself the news; the head only
		// says where it was observed.
		complete = true
	default:
		return parsed, false
	}
	return parsed, complete && parsed.repo != "" && parsed.prNumber > 0
}

func (p prCoherenceEvent) injectionBody(eventType string) string {
	target := fmt.Sprintf("PR #%d (%s)", p.prNumber, p.repo)
	head := domain.ShortSHA(p.headSHA)
	// On a CI or conflict note the head says where the change was observed
	// rather than what changed, so an absent one drops the clause instead of
	// leaving "at ." where a SHA belongs.
	at := ""
	if head != "" {
		at = " at " + head
	}
	switch eventType {
	case domain.EventGitHubPRNewCommits:
		advance := "advanced to " + head
		if from := domain.ShortSHA(p.oldHeadSHA); from != "" {
			advance = "advanced from " + from + " to " + head
		}
		return fmt.Sprintf("%s %s. Re-pull the branch and reconcile your work with the new head.", target, advance)
	case domain.EventGitHubPRCICheckFailed:
		body := fmt.Sprintf("CI check %q failed on %s%s. Inspect the failure before relying on the current result.", p.checkName, target, at)
		if p.checkURL != "" {
			body += " Check details: " + p.checkURL
		}
		return body
	case domain.EventGitHubPRCICheckPassed:
		return fmt.Sprintf("CI check %q is now %s on %s%s. Reconcile this recovery with any CI-fix work in progress.", p.checkName, p.conclusion, target, at)
	case domain.EventGitHubPRConflicts:
		return fmt.Sprintf("%s is now in GitHub's CONFLICTING merge state%s. Refresh the base and resolve the merge conflicts before finalizing.", target, at)
	default:
		return ""
	}
}

// HandlePRCoherence waits for the router's disposition sentinel, then sends an
// informational PR-state note to every relevant live or resumable delegation
// conversation. Waiting for disposition keeps this subscriber orthogonal to
// routing and lets it suppress the exact conversation already served by the
// same-task additive path without touching task_events itself.
func (s *Spawner) HandlePRCoherence(evt domain.Event) {
	if evt.EventType != domain.EventSystemRoutingDisposition || s.events == nil || s.conversations == nil {
		return
	}
	var disposition events.SystemRoutingDispositionMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &disposition); err != nil || disposition.Disposition == events.DispositionError {
		return
	}
	if _, ok := coherenceEventTypes[disposition.EventType]; !ok {
		return
	}

	ctx := context.Background()
	original, err := s.events.GetSystem(ctx, evt.OrgID, disposition.EventID)
	if err != nil || original == nil {
		delegateLog.Warn("PR coherence injection: load source event failed", "event", disposition.EventID, "error", err)
		return
	}
	entityID := disposition.EntityID
	if original.EntityID != nil {
		entityID = *original.EntityID
	}
	if entityID == "" {
		delegateLog.Warn("PR coherence injection: source event has no entity", "event", disposition.EventID)
		return
	}
	parsed, ok := parsePRCoherenceEvent(original)
	if !ok {
		delegateLog.Warn("PR coherence injection: source metadata unusable", "event", disposition.EventID, "event_type", disposition.EventType)
		return
	}

	headRepo, headRef := parsed.repo, ""
	if s.entities != nil {
		entity, loadErr := s.entities.GetSystem(ctx, evt.OrgID, entityID)
		if loadErr != nil {
			delegateLog.Warn("PR coherence injection: load entity snapshot failed", "entity", entityID, "error", loadErr)
		} else if entity != nil {
			var snapshot domain.PRSnapshot
			if json.Unmarshal([]byte(entity.SnapshotJSON), &snapshot) == nil {
				headRef = snapshot.HeadRef
				if strings.TrimSpace(snapshot.HeadRepo) != "" {
					headRepo = snapshot.HeadRepo
				}
			}
		}
	}
	branchRef := ""
	if headRef != "" {
		branchRef = worktree.CheckoutRefSlug(headRef)
	}
	targets, err := s.conversations.ListPRCoherenceTargetsSystem(ctx, evt.OrgID, db.PRCoherenceTargetQuery{
		EntityID:     entityID,
		EventID:      original.ID,
		BaseRepo:     parsed.repo,
		HeadRepo:     headRepo,
		ReviewTarget: domain.ReviewTarget(parsed.repo, parsed.prNumber),
		PRRef:        worktree.PRRefSlug(parsed.prNumber),
		BranchRef:    branchRef,
	})
	if err != nil {
		delegateLog.Warn("PR coherence injection: list conversations failed", "entity", entityID, "error", err)
		return
	}
	// A head change is only news to an agent that is actually behind it. The
	// gate is specific to this event type: a CI result or a conflict is news
	// whatever the checkout is anchored at.
	var anchored map[string]bool
	if original.EventType == domain.EventGitHubPRNewCommits {
		anchored = s.reviewsAnchoredAtHead(ctx, evt.OrgID, domain.ReviewTarget(parsed.repo, parsed.prNumber), parsed.headSHA)
	}

	body := parsed.injectionBody(original.EventType)
	for _, target := range targets {
		if target.ConversationID == disposition.InjectedConversationID {
			continue
		}
		if anchored[target.ConversationID] {
			continue
		}
		live := target.Active || s.getProc(target.ConversationID) != nil
		if !live && !injectionWillFlush(target.Status, target.Outcome) {
			continue
		}
		s.StageOrDeliverInformationalEvent(ctx, evt.OrgID, target.ConversationID, domain.StagedInjectionProducerPRCoherence, body,
			domain.NoteProvenance{EventID: original.ID, EventType: original.EventType})
	}
}

// reviewsAnchoredAtHead returns the conversations whose pending review already
// records headSHA — via its start anchor or a comment finalize-review
// reconciled forward — and which therefore are not behind the advance. The
// review's own recorded anchors are the observable stand-in for the agent's
// worktree HEAD, which TF does not track as a column and which may live on
// another executor.
//
// Best-effort: a lookup failure returns no suppressions, so every target gets
// the note. A spurious "re-pull" is harmless; a missed advance leaves the
// agent working against a diff that moved.
func (s *Spawner) reviewsAnchoredAtHead(ctx context.Context, orgID, target, headSHA string) map[string]bool {
	if s.artifacts == nil || headSHA == "" {
		return nil
	}
	reviews, err := s.artifacts.ListPendingReviewsByTargetSystem(ctx, orgID, target)
	if err != nil {
		delegateLog.Warn("PR coherence injection: lookup pending reviews failed", "target", target, "error", err)
		return nil
	}
	anchored := map[string]bool{}
	for i := range reviews {
		if reviews[i].ConversationID != "" && domain.ReviewAnchoredAtHead(reviews[i], headSHA) {
			anchored[reviews[i].ConversationID] = true
		}
	}
	return anchored
}

var coherenceEventTypes = map[string]struct{}{
	domain.EventGitHubPRNewCommits:    {},
	domain.EventGitHubPRCICheckFailed: {},
	domain.EventGitHubPRCICheckPassed: {},
	domain.EventGitHubPRConflicts:     {},
}
