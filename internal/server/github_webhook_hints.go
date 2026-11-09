package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// hintedEvent reports whether eventName is one of the content events TF's
// App subscribes to (the manifest's default_events, less the installation
// events), each of which names what changed.
func hintedEvent(eventName string) bool {
	switch eventName {
	case "pull_request", "pull_request_review", "pull_request_review_comment",
		"issue_comment", "check_run", "check_suite", "push":
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
	}

	for _, target := range h.targets {
		if err := target.Validate(); err != nil {
			return nil, fmt.Errorf("%s payload: %w", eventName, err)
		}
	}
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
// delivery. repositoryID is the tracked registry row the hint's repository
// resolved to, or "" for a delivery that marks nothing — no hint, no targets,
// or a repository no team tracks — which is recorded all the same.
// fresh=false is a duplicate: nothing was written. marked counts the marks
// written.
func (s *Server) recordContentDelivery(ctx context.Context, orgID string, d verifiedDelivery, repositoryID string) (fresh bool, marked int, err error) {
	err = s.tx.SystemWithTx(ctx, orgID, func(tx db.SystemTxStores) error {
		marked = 0
		var err error
		if fresh, err = tx.GitHubDeliveries.MarkDeliveredSystem(ctx, d.installationID, d.deliveryID); err != nil {
			return fmt.Errorf("record delivery: %w", err)
		}
		if !fresh || repositoryID == "" {
			return nil
		}
		for _, target := range d.hint.targets {
			if _, err := tx.GitHubRefreshMarks.MarkSystem(ctx, orgID, repositoryID, target); err != nil {
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

// trackedHintRepository resolves a hint's repository to the tracked registry
// row on host, or "" when there is nothing to mark: no hint, a hint naming no
// target, or a repository with no row or one no team tracks. No row is
// created: a repository TF holds nothing for is one the poll does not poll.
func (s *Server) trackedHintRepository(ctx context.Context, orgID, host string, hint *refreshHint) (string, error) {
	if hint == nil || len(hint.targets) == 0 {
		return "", nil
	}
	repo, err := s.repos.GetTrackedByExternalIDSystem(ctx, orgID, host, hint.repositoryExternalID)
	if err != nil {
		return "", fmt.Errorf("resolve hinted repository: %w", err)
	}
	if repo == nil {
		return "", nil
	}
	return repo.ID, nil
}

// pruneWebhookDeliveries sweeps expired delivery records after a delivery is
// recorded. Best-effort: a missed prune grows the table a little until the
// next one succeeds, which is never a reason to fail the delivery.
func (s *Server) pruneWebhookDeliveries(ctx context.Context) {
	if err := s.githubDeliveries.PruneDeliveredSystem(ctx); err != nil {
		githubAppLog.Warn("github webhook delivery prune failed", "error", err)
	}
}
