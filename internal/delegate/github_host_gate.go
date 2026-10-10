package delegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// errGitHubHostMismatch is the refusal of a GitHub run whose pull request is
// not on the org's current GitHub host. Every GitHub path a run takes — the
// credential resolve, the pull request fetch, the clone, a workspace restore —
// goes to the org's current host, so a run on a pull request from another host
// would read the current host's same-named repository and pull request into the
// old host's bare clone. Re-reading the same two rows gives the same answer, so
// it is settled rather than retried.
var errGitHubHostMismatch = errors.New("the pull request is not on the org's GitHub host")

// gitHubHostMismatchError carries the two hosts a refusal is about.
type gitHubHostMismatchError struct {
	// PRHost is the GitHub host the task's pull request is on (its entity's
	// scope). Empty when the entity records none.
	PRHost string
	// CurrentHost is the org's GitHub host now. Empty when none is
	// configured.
	CurrentHost string
}

func (e *gitHubHostMismatchError) Error() string {
	switch {
	case e.PRHost == "":
		return fmt.Sprintf("%s: the pull request records no GitHub host, and the org's GitHub host is %s", errGitHubHostMismatch, hostOrNone(e.CurrentHost))
	case e.CurrentHost == "":
		return fmt.Sprintf("%s: the pull request is on %s, and the org has no GitHub host configured", errGitHubHostMismatch, e.PRHost)
	}
	return fmt.Sprintf("%s: the pull request is on %s, and the org's GitHub host is now %s", errGitHubHostMismatch, e.PRHost, e.CurrentHost)
}

func (e *gitHubHostMismatchError) Unwrap() error { return errGitHubHostMismatch }

func hostOrNone(host string) string {
	if host == "" {
		return "not configured"
	}
	return host
}

// githubHostRefusal compares a GitHub task's pull request host with the org's
// current GitHub host and returns the refusal when they differ. Any other task
// passes. err is a failed read, which says nothing about the hosts and is
// retried like any other setup failure.
//
// An empty current host is "not configured" and refuses: there is no host to
// run the pull request on.
func (s *Spawner) githubHostRefusal(ctx context.Context, orgID string, task domain.Task) (*gitHubHostMismatchError, error) {
	if task.EntitySource != "github" {
		return nil, nil
	}
	if s.entities == nil || s.orgs == nil {
		return nil, errors.New("check the pull request's GitHub host: no entity or org store")
	}
	entity, err := s.entities.GetSystem(ctx, orgID, task.EntityID)
	if err != nil {
		return nil, fmt.Errorf("check the pull request's GitHub host: read entity: %w", err)
	}
	if entity == nil {
		return nil, fmt.Errorf("check the pull request's GitHub host: entity %s not found", task.EntityID)
	}
	current, err := db.OrgGitHubHostSystem(ctx, s.orgs, orgID)
	if err != nil {
		return nil, fmt.Errorf("check the pull request's GitHub host: %w", err)
	}
	if entity.Scope != "" && entity.Scope == current {
		return nil, nil
	}
	return &gitHubHostMismatchError{PRHost: entity.Scope, CurrentHost: current}, nil
}

// disposeOfHostRefusal answers for a claim on a pull request the org's GitHub
// host no longer serves. It has disposeOfModelRefusal's shape — park what has
// a transcript, fail a step that never ran — reached without the retries.
//
// A parked conversation takes launch_failed, the reason for a run that could
// not start; the note says why, and that a message wakes it into the same
// refusal until the org is on the pull request's host again.
func (s *Spawner) disposeOfHostRefusal(orgID string, br *domain.BlueprintRun, conv domain.Conversation, cause *gitHubHostMismatchError) {
	if br == nil || s.conversationHasWork(orgID, conv.ID) {
		dispatchLog.Warn("claim refused: the pull request is not on the org's GitHub host; parking",
			"conversation", conv.ID, "pr_host", cause.PRHost, "current_host", cause.CurrentHost)
		s.parkWithStopNote(orgID, conv, domain.ParkReasonLaunchFailed,
			fmt.Sprintf("This conversation cannot continue: %s. A run reaches GitHub only on the org's current host.", cause),
			fmt.Sprintf("Run %s is paused: %s", shortConversationID(conv.ID), truncateToastMsg(cause.Error(), 160)))
		return
	}
	dispatchLog.Error("blueprint step refused: the pull request is not on the org's GitHub host",
		"conversation", conv.ID, "blueprint_run", br.ID, "pr_host", cause.PRHost, "current_host", cause.CurrentHost)
	s.failUnstartedStep(orgID, br, conv, cause.Error())
}
