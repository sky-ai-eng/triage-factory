package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// refreshHint is what a content delivery says changed: a repository, by
// GitHub's id for it, and the targets in that repository for the poll to
// refresh. A delivery is a hint and never an event: it carries no content TF
// acts on, so what it names is refreshed with TF's own credential and the
// tracker's snapshot diff decides what happened.
type refreshHint struct {
	repositoryExternalID string
	targets              []domain.GitHubRefreshTarget
}

// hintPayload is the subset of a content delivery's payload a hint is read
// from. GitHub sends each block only on the events that carry it.
type hintPayload struct {
	Action     string `json:"action"`
	Repository *struct {
		ID int64 `json:"id"`
	} `json:"repository"`
	PullRequest *struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	CheckRun   *hintCheck `json:"check_run"`
	CheckSuite *hintCheck `json:"check_suite"`
	Ref        string     `json:"ref"`
	// Alert is read only for its presence: what changed on it is left to the
	// listing.
	Alert *struct{} `json:"alert"`
}

// hintCheck is the part of a check_run or check_suite object a hint reads:
// the commit it ran on, and the pull requests GitHub matched to that commit.
// The list is empty for a pull request from a fork, which is why the head SHA
// is marked too.
type hintCheck struct {
	HeadSHA      string `json:"head_sha"`
	PullRequests []struct {
		Number int `json:"number"`
		Base   struct {
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_requests"`
}

// hintedEvent reports whether eventName is a content event TF reads a refresh
// hint from, each of which names what changed. They are every content event
// the App subscribes to.
func hintedEvent(eventName string) bool {
	switch eventName {
	case "pull_request", "pull_request_review", "pull_request_review_comment",
		"issue_comment", "check_run", "check_suite", "push", "dependabot_alert":
		return true
	}
	return false
}

// parseRefreshHint reads the hint a verified content delivery carries:
//
//   - pull_request, pull_request_review, pull_request_review_comment: the
//     pull request.
//   - issue_comment: the pull request, when the issue is one; a comment on an
//     issue names nothing.
//   - check_run, check_suite: the head SHA, and each listed pull request in
//     the delivery's own repository.
//   - push: the branch; a tag push names nothing.
//   - dependabot_alert: the repository's alerts, which one listing reads
//     whichever alert the delivery is about. An assignees_changed delivery
//     names nothing: who an alert is assigned to is not something TF records,
//     and it opens or closes no alert.
//
// A hint naming nothing has no targets. Every error is a property of the
// payload alone — it does not parse, it names no repository, or a value it
// names is not one GitHub writes — so it is the same answer on every attempt,
// which is what lets the caller refuse the delivery ahead of the dedup gate.
func parseRefreshHint(eventName string, body []byte) (*refreshHint, error) {
	var p hintPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("malformed %s payload", eventName)
	}
	if p.Repository == nil || p.Repository.ID <= 0 {
		return nil, fmt.Errorf("%s payload missing repository id", eventName)
	}
	h := &refreshHint{repositoryExternalID: strconv.FormatInt(p.Repository.ID, 10)}

	switch eventName {
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		if p.PullRequest == nil {
			return nil, fmt.Errorf("%s payload missing pull_request", eventName)
		}
		h.add(domain.PullRequestRefreshTarget(p.PullRequest.Number))
	case "issue_comment":
		if p.Issue == nil {
			return nil, errors.New("issue_comment payload missing issue")
		}
		if pr := bytes.TrimSpace(p.Issue.PullRequest); len(pr) > 0 && !bytes.Equal(pr, []byte("null")) {
			h.add(domain.PullRequestRefreshTarget(p.Issue.Number))
		}
	case "check_run", "check_suite":
		check := p.CheckRun
		if eventName == "check_suite" {
			check = p.CheckSuite
		}
		if check == nil {
			return nil, fmt.Errorf("%s payload missing %s", eventName, eventName)
		}
		h.add(domain.HeadSHARefreshTarget(check.HeadSHA))
		for _, pr := range check.PullRequests {
			// A listed pull request whose base is another repository is
			// numbered in that repository, not in this one.
			if base := pr.Base.Repo.ID; base != 0 && base != p.Repository.ID {
				continue
			}
			h.add(domain.PullRequestRefreshTarget(pr.Number))
		}
	case "push":
		if p.Ref == "" {
			return nil, errors.New("push payload missing ref")
		}
		if branch, ok := strings.CutPrefix(p.Ref, "refs/heads/"); ok {
			h.add(domain.BranchRefreshTarget(branch))
		}
	case "dependabot_alert":
		if p.Alert == nil {
			return nil, errors.New("dependabot_alert payload missing alert")
		}
		if p.Action != "assignees_changed" {
			h.add(domain.DependabotAlertsRefreshTarget())
		}
	}

	for _, target := range h.targets {
		if err := target.Validate(); err != nil {
			return nil, fmt.Errorf("%s payload: %w", eventName, err)
		}
	}
	// One order for every delivery, so two transactions writing overlapping
	// marks take the rows' locks in the same order and cannot deadlock.
	slices.SortFunc(h.targets, func(a, b domain.GitHubRefreshTarget) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Value, b.Value))
	})
	return h, nil
}

// add appends target unless the hint already names it, so a check run listing
// one pull request twice is one mark.
func (h *refreshHint) add(target domain.GitHubRefreshTarget) {
	for _, have := range h.targets {
		if have == target {
			return
		}
	}
	h.targets = append(h.targets, target)
}

// recordContentDelivery records a verified delivery that is not an
// installation event, and writes the refresh marks it stands for in the same
// transaction: a delivery is never recorded as handled without its marks, and
// a failed mark write rolls the record back so a redelivery applies the
// delivery. marks is what hintMarks resolved the delivery's hint to; a
// delivery that marks nothing is recorded all the same. fresh=false is a
// duplicate: nothing was written. marked counts the marks written.
func (s *Server) recordContentDelivery(ctx context.Context, orgID string, d verifiedDelivery, marks hintedMarks) (fresh bool, marked int, err error) {
	err = s.tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
		marked = 0
		var err error
		if fresh, err = tx.GitHubDeliveries.MarkDeliveredSystem(ctx, d.installationID, d.deliveryID); err != nil {
			return fmt.Errorf("record delivery: %w", err)
		}
		if !fresh {
			return nil
		}
		for _, target := range marks.targets {
			if _, err := tx.GitHubRefreshMarks.MarkSystem(ctx, orgID, marks.repositoryID, target); err != nil {
				return fmt.Errorf("refresh mark %s %s: %w", target.Kind, target.Value, err)
			}
			marked++
		}
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return fresh, marked, nil
}

// hintedMarks is the refresh marks a delivery writes: targets, in the tracked
// registry row repositoryID. The zero value writes none.
type hintedMarks struct {
	repositoryID string
	targets      []domain.GitHubRefreshTarget
}

// hintMarks resolves a hint to the marks it writes: its repository's tracked
// registry row on host, and the targets in it the poll refreshes. It writes
// none for no hint, a hint naming no target, or a repository with no row or
// one no team tracks. No row is created: a repository TF holds nothing for is
// one the poll does not poll. The repository's Dependabot alerts are marked
// only when the poll lists them, which is when the recorded answer says they
// can be read; for any other answer, or none yet, the listing a mark asks for
// is one the poll does not make.
func (s *Server) hintMarks(ctx context.Context, orgID, host string, hint *refreshHint) (hintedMarks, error) {
	if hint == nil || len(hint.targets) == 0 {
		return hintedMarks{}, nil
	}
	repo, err := s.repos.GetTrackedByExternalIDSystem(ctx, orgID, host, hint.repositoryExternalID)
	if err != nil {
		return hintedMarks{}, fmt.Errorf("resolve hinted repository: %w", err)
	}
	if repo == nil {
		return hintedMarks{}, nil
	}
	targets := hint.targets
	if slices.ContainsFunc(targets, domain.GitHubRefreshTarget.DependabotAlerts) {
		listed, err := s.dependabotAlertsListed(ctx, orgID, repo.ID)
		if err != nil {
			return hintedMarks{}, err
		}
		if !listed {
			targets = slices.DeleteFunc(slices.Clone(targets), domain.GitHubRefreshTarget.DependabotAlerts)
		}
	}
	if len(targets) == 0 {
		return hintedMarks{}, nil
	}
	return hintedMarks{repositoryID: repo.ID, targets: targets}, nil
}

// dependabotAlertsListed reports whether the poll lists the Dependabot alerts
// of the repository whose registry row is repositoryID: its recorded answer
// says they can be read.
func (s *Server) dependabotAlertsListed(ctx context.Context, orgID, repositoryID string) (bool, error) {
	if s.dependabotAccess == nil {
		return false, nil
	}
	answer, err := s.dependabotAccess.GetSystem(ctx, orgID, repositoryID)
	if err != nil {
		return false, fmt.Errorf("read hinted repository's dependabot access: %w", err)
	}
	return answer.Listed(), nil
}

// pruneWebhookDeliveries sweeps expired delivery records after a delivery is
// recorded. Best-effort: a missed prune grows the table a little until the
// next one succeeds, which is never a reason to fail the delivery.
func (s *Server) pruneWebhookDeliveries(ctx context.Context) {
	if err := s.githubDeliveries.PruneDeliveredSystem(ctx); err != nil {
		githubAppLog.Warn("github webhook delivery prune failed", "error", err)
	}
}
