package commitsigner

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// PeriodicCheckInterval is the least time between two periodic checks of one
// signer. A key removed from the account is found within it, plus a poll
// interval; so is one put back.
const PeriodicCheckInterval = 15 * time.Minute

const (
	// postPushWindow is how long after its push a commit GitHub has not
	// answered for is still asked about. Past it the commit says little about
	// the signer as it is now.
	postPushWindow = 24 * time.Hour
	// postPushBatch bounds the pushed commits one cycle reads for one org.
	postPushBatch = 20
	// healthCheckTimeout bounds one org's checks.
	healthCheckTimeout = 2 * time.Minute
)

// HealthChecker makes the checks of an org's signer between an admin's
// Verifies (spec §7), so a signer whose commits GitHub would mark Unverified
// stops signing at the next signature:
//
//   - The periodic check, at most every PeriodicCheckInterval: it reads the
//     account by its id, refreshes the signer's login and profile name, and
//     looks for the key among the account's signing keys. A missing key breaks
//     an active signer (key_missing), and the same check restores it once the
//     key is back.
//   - The post-push check: for each commit a push landed since the last cycle,
//     it reads GitHub's verification and stores the reason on the signature.
//     A failure that still says something about the signer as it is now
//     (FailureHolds) breaks it with that reason, which only a Verify clears.
//
// Both use the org's own GitHub credential and run on the GitHub poll cycle:
// in local mode in the TF process, in multi mode on the control pod holding
// the brain lease. A check that cannot read GitHub changes nothing and runs
// again on the next cycle.
type HealthChecker struct {
	Stores db.Stores
	GitHub ghclient.Resolver

	// now replaces time.Now when set. Tests move the clock.
	now func() time.Time

	running sync.Map // org id → struct{}, while that org's checks run
}

func (h *HealthChecker) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// Trigger runs orgID's checks in the background, unless a run of them for the
// org is still going: the next poll cycle triggers again, so a trigger dropped
// then costs nothing. ctx bounds the run, which also has a timeout of its own.
func (h *HealthChecker) Trigger(ctx context.Context, orgID string) {
	if orgID == "" {
		return
	}
	if _, busy := h.running.LoadOrStore(orgID, struct{}{}); busy {
		return
	}
	go func() {
		defer h.running.Delete(orgID)
		ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
		defer cancel()
		h.Check(ctx, orgID)
	}()
}

// Check runs orgID's checks once: the periodic check when the signer is due
// one, then the post-push check. An org with no signer has nothing to check.
func (h *HealthChecker) Check(ctx context.Context, orgID string) {
	signer, err := h.Stores.OrgCommitSigners.GetSystem(ctx, orgID)
	if err != nil {
		signerLog.ErrorContext(ctx, "commit signer health check could not read the signer", "org", orgID, "error", err)
		return
	}
	// TODO(TFAC-1200): check the deployment signer too, on the control pods,
	// once one can be configured: its key on its account, and the pushed
	// commits of the managed workspaces that sign as it.
	if signer == nil {
		return
	}
	current := h.checkKey(ctx, *signer)
	h.checkPushes(ctx, current)
}

// checkKey is the periodic check. It returns the signer as the check left it.
func (h *HealthChecker) checkKey(ctx context.Context, signer domain.OrgCommitSigner) domain.OrgCommitSigner {
	if signer.CheckedAt != nil && h.clock().Sub(*signer.CheckedAt) < PeriodicCheckInterval {
		return signer
	}
	check, err := h.readKey(ctx, signer)
	if err != nil {
		CountCheck(ctx, CheckPeriodic, CheckResultError)
		signerLog.WarnContext(ctx, "commit signer periodic check could not read GitHub; it runs again next cycle",
			"org", signer.OrgID, "error", err)
		return signer
	}
	result := domain.CommitVerificationValid
	if !check.KeyFound {
		result = CheckResultKeyMissing
	}
	CountCheck(ctx, CheckPeriodic, result)
	updated, err := h.Stores.OrgCommitSigners.RecordKeyCheckSystem(ctx, signer.OrgID, check)
	if err != nil {
		signerLog.ErrorContext(ctx, "commit signer periodic check could not be recorded", "org", signer.OrgID, "error", err)
		return signer
	}
	if updated == nil {
		// Removed, registered again or rotated while the check was out: the
		// stored signer is newer than anything this check read.
		return signer
	}
	logTransition(ctx, CheckPeriodic, signer, *updated)
	return *updated
}

// readKey reads the signer's account by its id and its signing keys. An
// account GitHub no longer has is one whose keys are gone too, and its login
// and name are kept as stored.
func (h *HealthChecker) readKey(ctx context.Context, signer domain.OrgCommitSigner) (db.SignerKeyCheck, error) {
	check := db.SignerKeyCheck{GitHubUserID: signer.GitHubUserID, PublicKey: signer.PublicKey}
	client, err := ghclient.AccountClient(ctx, h.GitHub, h.Stores.Orgs, h.Stores.GitHubApps, signer.OrgID)
	if err != nil {
		return check, fmt.Errorf("resolve the org's GitHub client: %w", err)
	}
	account, err := client.GetAccountByID(ctx, strconv.FormatInt(signer.GitHubUserID, 10))
	if ghclient.IsHTTP404(err) {
		return check, nil
	}
	if err != nil {
		return check, fmt.Errorf("read the signer's account: %w", err)
	}
	keys, err := client.ListSSHSigningKeys(ctx, account.Login)
	if err != nil {
		return check, fmt.Errorf("read the account's signing keys: %w", err)
	}
	check.Login, check.Name = account.Login, account.Name
	check.KeyFound = slices.Contains(keys, signer.PublicKey)
	return check, nil
}

// checkPushes is the post-push check of the org signer's pushed commits.
func (h *HealthChecker) checkPushes(ctx context.Context, signer domain.OrgCommitSigner) {
	pushed, err := h.Stores.CommitSignatures.UncheckedPushesSystem(ctx, signer.OrgID, domain.CommitSigningOrg,
		h.clock().Add(-postPushWindow), postPushBatch)
	if err != nil {
		signerLog.ErrorContext(ctx, "commit signer post-push check could not read the pushed commits", "org", signer.OrgID, "error", err)
		return
	}
	for _, sig := range pushed {
		verdict, found, err := h.readVerification(ctx, sig)
		if err != nil {
			CountCheck(ctx, CheckPostPush, CheckResultError)
			signerLog.WarnContext(ctx, "commit signer post-push check could not read GitHub; it runs again next cycle",
				"org", signer.OrgID, "signature", sig.ID, "error", err)
			continue
		}
		reason := ""
		if found {
			reason = verdict.Reason
			CountCheck(ctx, CheckPostPush, reason)
		}
		// The break is written before the verdict, so a failure to write it
		// leaves the commit unchecked and the next cycle tries again.
		if found && reason != "" && FailureHolds(signer, signer.PublicKey, sig, verdict) {
			broken, err := h.Stores.OrgCommitSigners.BreakSystem(ctx, signer.OrgID, sig.PublicKey, reason)
			if err != nil {
				signerLog.ErrorContext(ctx, "commit signer post-push check could not break the signer", "org", signer.OrgID, "error", err)
				continue
			}
			if broken != nil {
				logTransition(ctx, CheckPostPush, signer, *broken)
				signer = *broken
			}
		}
		if _, err := h.Stores.CommitSignatures.SetVerificationSystem(ctx, signer.OrgID, sig.ID, reason); err != nil {
			signerLog.ErrorContext(ctx, "commit signer post-push check could not record GitHub's answer",
				"org", signer.OrgID, "signature", sig.ID, "error", err)
		}
	}
}

// readVerification reads sig's pushed commit's verification from GitHub.
// found is false when there is no answer to give: the repository is gone from
// the registry, or GitHub no longer has the commit or the repository.
func (h *HealthChecker) readVerification(ctx context.Context, sig domain.CommitSignature) (ghclient.CommitVerification, bool, error) {
	if sig.RepositoryID == "" || sig.CommitSHA == "" {
		return ghclient.CommitVerification{}, false, nil
	}
	repo, err := h.Stores.Repos.GetSystem(ctx, sig.OrgID, sig.RepositoryID)
	if err != nil {
		return ghclient.CommitVerification{}, false, fmt.Errorf("read the pushed commit's repository: %w", err)
	}
	if repo == nil {
		return ghclient.CommitVerification{}, false, nil
	}
	client, err := ghclient.ClientForOwner(ctx, h.GitHub, sig.OrgID, repo.OwnerExternalID, repo.Owner)
	if err != nil {
		return ghclient.CommitVerification{}, false, fmt.Errorf("resolve a GitHub client for the repository: %w", err)
	}
	verdict, err := client.GetCommitVerification(ctx, repo.Owner, repo.Repo, sig.CommitSHA)
	if ghclient.IsHTTP404(err) {
		return ghclient.CommitVerification{}, false, nil
	}
	if err != nil {
		return ghclient.CommitVerification{}, false, err
	}
	return verdict, true, nil
}

// FailureHolds reports whether a pushed commit GitHub does not verify still
// says something about signer as it is now, signing with key. GitHub evaluates
// a failing result again on every read, so the commit reads valid once its
// fault is fixed; but one signed as another account or with another key, or
// committed under an address the signer no longer commits as, fails for a
// fault that a re-registration, a rotation or an email change has already
// replaced, and holding the signer on it would keep it broken for good.
func FailureHolds(signer domain.OrgCommitSigner, key string, sig domain.CommitSignature, verdict ghclient.CommitVerification) bool {
	return !verdict.Verified &&
		sig.PublicKey == key &&
		sig.SignerUserID == signer.GitHubUserID &&
		db.NormalizeGitHubHost(sig.SignerHost) == db.NormalizeGitHubHost(signer.Host) &&
		strings.EqualFold(verdict.CommitterEmail, signer.CommitEmail())
}

// logTransition logs a check that moved the signer's status: a WARN when it
// broke, since every run of the org commits unsigned from its next signature
// on.
func logTransition(ctx context.Context, check string, before, after domain.OrgCommitSigner) {
	if before.Status == after.Status && before.StatusReason == after.StatusReason {
		return
	}
	if after.Status == domain.CommitSignerBroken {
		signerLog.WarnContext(ctx, "commit signer is broken; the org's runs commit unsigned until an admin fixes it",
			"org", after.OrgID, "check", check, "reason", after.StatusReason, "was", before.Status)
		return
	}
	signerLog.InfoContext(ctx, "commit signer recovered", "org", after.OrgID, "check", check, "status", after.Status)
}
