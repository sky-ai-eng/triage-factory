package commitsigner

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// DeploymentWatchInterval is how often every control pod reads its deployment
// signer's health: what its gauge reports and its WARN fires on, and the tick
// a periodic check runs on once one is due.
const DeploymentWatchInterval = 30 * time.Second

// FirstTickWait is how long a control pod's boot waits for its first tick
// (WaitFirstTick) before it signs or seals anything.
const FirstTickWait = 5 * time.Second

// DeploymentHealth keeps the deployment signer's health on one control pod. It
// runs on every control pod, whether or not it holds the brain lease, because
// every pod signs and every pod reports what it would sign with:
//
//   - It registers the pod's configured key, creating its
//     deployment_commit_signer row pending on first boot.
//   - Every DeploymentWatchInterval it reads that row, reports it as
//     tf_deployment_commit_signer_up, and logs a WARN when it finds the signer
//     broken, so an operator watching any pod's log sees it.
//   - When the row's last check is PeriodicCheckInterval old, or it has
//     never been checked, it makes the periodic check: it reads the account
//     (by the configured login the first time, by id after), refreshes its
//     login and name, and looks for the key among its signing keys. Pods race
//     for a due check harmlessly; the write is a compare-and-set, and the row
//     is due again only an interval later.
//
// The account reads carry no credential: an account and its signing keys are
// public on GitHub, and the deployment signer belongs to no org whose
// credential the check could borrow. Verify, an operator's action
// (`triagefactory commit-signer verify`), makes the same check on demand and,
// for a signer a pushed commit broke, re-reads that commit with its org's
// credential. The post-push check itself runs per org on the brain
// (HealthChecker), because each pushed commit is in an org's repository.
type DeploymentHealth struct {
	Signer *DeploymentSigner
	Stores db.Stores
	// GitHub resolves an org's credential, for Verify's re-read of a pushed
	// commit in that org's repository.
	GitHub ghclient.Resolver

	client *ghclient.Client
	now    func() time.Time

	// failures and retryAt pace this pod's periodic check after one fails. A
	// failed check writes nothing, so the row stays due, and without them
	// every tick would read GitHub again. Only Tick touches them.
	failures int
	retryAt  time.Time

	// firstTick is closed when the first Tick returns.
	firstTick     chan struct{}
	firstTickOnce sync.Once

	mu       sync.Mutex
	observed *domain.DeploymentCommitSigner
	reg      metric.Registration
}

// NewDeploymentHealth returns signer's health keeper and registers its gauge
// against provider. A provider that cannot build the instrument leaves it
// unregistered, and the watch still runs.
func NewDeploymentHealth(provider metric.MeterProvider, signer *DeploymentSigner, stores db.Stores, gh ghclient.Resolver) *DeploymentHealth {
	d := &DeploymentHealth{Signer: signer, Stores: stores, GitHub: gh, client: ghclient.NewClient(signer.Host, ""), firstTick: make(chan struct{})}
	up, err := provider.Meter(meterName).Int64ObservableGauge("deployment_commit_signer.up",
		metric.WithDescription("1 while the deployment commit signer is active, 0 while it is not."))
	if err != nil {
		signerLog.Error("deployment commit signer gauge setup failed", "error", err)
		return d
	}
	reg, err := provider.Meter(meterName).RegisterCallback(func(_ context.Context, o metric.Observer) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		// Nothing before the first read: a 0 this pod has not measured would
		// page an operator for a signer that may be fine.
		if d.observed != nil {
			v := int64(0)
			if d.observed.Status == domain.CommitSignerActive {
				v = 1
			}
			o.ObserveInt64(up, v)
		}
		return nil
	}, up)
	if err != nil {
		signerLog.Error("deployment commit signer gauge callback registration failed", "error", err)
		return d
	}
	d.reg = reg
	return d
}

func (d *DeploymentHealth) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// Run ticks now and every interval until ctx is cancelled, then unregisters
// the gauge.
func (d *DeploymentHealth) Run(ctx context.Context, interval time.Duration) {
	defer d.close()
	d.Tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Tick(ctx)
		}
	}
}

func (d *DeploymentHealth) close() {
	d.mu.Lock()
	reg := d.reg
	d.reg = nil
	d.mu.Unlock()
	if reg != nil {
		if err := reg.Unregister(); err != nil {
			signerLog.Error("deployment commit signer gauge callback unregister failed", "error", err)
		}
	}
}

// WaitFirstTick waits up to timeout for the first Tick to return, and reports
// whether it did. A control pod waits on it before it signs or seals: a key the
// deployment has never checked has no account until a check finds it, so a pod
// that started on a new key would, until its first check, answer a managed
// org as though it had no signer, and a run that asked in that second
// would stop signing for the rest of the run. A first tick still reading
// GitHub at timeout carries on; the pod starts without it, as it would
// without this wait.
func (d *DeploymentHealth) WaitFirstTick(ctx context.Context, timeout time.Duration) bool {
	select {
	case <-d.firstTick:
		return true
	default:
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-d.firstTick:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// Tick reads the signer's health, reports it, and makes the periodic check
// when one is due. A read that fails keeps what the gauge last reported; the
// next tick reads again.
func (d *DeploymentHealth) Tick(ctx context.Context) {
	defer d.firstTickOnce.Do(func() { close(d.firstTick) })
	row, err := d.Stores.DeploymentCommitSigners.RegisterSystem(ctx, d.Signer.Host, d.Signer.PublicKey, d.Signer.Login)
	if err != nil {
		signerLog.ErrorContext(ctx, "reading the deployment commit signer failed; keeping the last status", "error", err)
		return
	}
	d.observe(ctx, row)
	if row.CheckedAt != nil && d.clock().Sub(*row.CheckedAt) < PeriodicCheckInterval {
		return
	}
	if d.clock().Before(d.retryAt) {
		return
	}
	d.checkKey(ctx, row)
}

// backOff records a failed periodic check and returns how long this pod waits
// before the next.
func (d *DeploymentHealth) backOff() time.Duration {
	d.failures++
	delay := checkRetryDelay(d.failures)
	d.retryAt = d.clock().Add(delay)
	return delay
}

// checkRetryDelay is how long a pod waits to check again after its failures-th
// failed read in a row: a minute, doubling up to PeriodicCheckInterval, so a
// GitHub refusing anonymous reads (a rate limit, a private-mode Enterprise
// Server) costs each pod at most one check per interval.
func checkRetryDelay(failures int) time.Duration {
	delay := time.Minute
	for i := 1; i < failures && delay < PeriodicCheckInterval; i++ {
		delay *= 2
	}
	return min(delay, PeriodicCheckInterval)
}

// observe records row as this pod's view of the signer and logs a move of its
// status: a WARN when it is broken, since every managed org with no
// signer of its own commits unsigned from its next signature on.
func (d *DeploymentHealth) observe(ctx context.Context, row domain.DeploymentCommitSigner) {
	d.mu.Lock()
	before := d.observed
	d.observed = &row
	d.mu.Unlock()
	if before != nil && before.Status == row.Status && before.StatusReason == row.StatusReason {
		return
	}
	switch row.Status {
	case domain.CommitSignerBroken:
		signerLog.WarnContext(ctx, "deployment commit signer is broken; managed organizations commit unsigned until an operator fixes it",
			"login", row.Login, "host", row.Host, "reason", row.StatusReason, "since", row.StatusSince)
	case domain.CommitSignerActive:
		signerLog.InfoContext(ctx, "deployment commit signer is active; managed organizations sign as it", "login", row.Login, "host", row.Host)
	case domain.CommitSignerPending:
		signerLog.InfoContext(ctx, "deployment commit signer is pending; managed organizations sign once a check finds its key on the account",
			"login", row.Login, "host", row.Host)
	}
}

// keyRead is one read of the signer's account and its signing keys. found is
// false for an account GitHub does not have, or that is not a user; id is the
// account's id when found.
type keyRead struct {
	account  ghclient.Account
	id       int64
	found    bool
	keyFound bool
}

// readKey reads the account row follows, by its id once a check has read it
// and by the configured login before, and its signing keys.
func (d *DeploymentHealth) readKey(ctx context.Context, row domain.DeploymentCommitSigner) (keyRead, error) {
	var (
		account ghclient.Account
		err     error
	)
	if row.GitHubUserID > 0 {
		account, err = d.client.GetAccountByID(ctx, strconv.FormatInt(row.GitHubUserID, 10))
	} else {
		account, err = d.client.GetAccountByLogin(ctx, d.Signer.Login)
	}
	if ghclient.IsHTTP404(err) {
		return keyRead{}, nil
	}
	if err != nil {
		return keyRead{}, fmt.Errorf("read the signer's account: %w", err)
	}
	if account.Type != ghclient.AccountTypeUser {
		return keyRead{account: account}, nil
	}
	id, err := strconv.ParseInt(account.ID, 10, 64)
	if err != nil {
		return keyRead{}, fmt.Errorf("read the signer's account id %q: %w", account.ID, err)
	}
	keys, err := d.client.ListSSHSigningKeys(ctx, account.Login)
	if err != nil {
		return keyRead{}, fmt.Errorf("read the account's signing keys: %w", err)
	}
	return keyRead{account: account, id: id, found: true, keyFound: slices.Contains(keys, d.Signer.PublicKey)}, nil
}

// loginMoved reports whether the account row follows by id now has a login
// other than the configured one: renamed on GitHub, or the setting changed to
// name another account. Either way the key keeps signing as the account a
// check first found it on.
func (d *DeploymentHealth) loginMoved(row domain.DeploymentCommitSigner, read keyRead) bool {
	return row.GitHubUserID > 0 && read.found && !strings.EqualFold(read.account.Login, d.Signer.Login)
}

// newKeyAdvice is what an operator does to sign as an account other than the
// one a key is bound to.
const newKeyAdvice = "a key stays with the account a check first found it on, so to sign as another account, generate a new key and set " + EnvDeploymentSignerKey

// checkKey is the periodic check.
func (d *DeploymentHealth) checkKey(ctx context.Context, row domain.DeploymentCommitSigner) {
	read, err := d.readKey(ctx, row)
	if err != nil {
		CountCheck(ctx, CheckPeriodic, CheckResultError)
		msg := "deployment commit signer periodic check could not read GitHub; this pod tries again after retry_in"
		if ghclient.IsHTTP404(err) {
			msg = "the deployment commit signer's GitHub does not serve SSH signing keys, so it cannot verify a commit TF signs"
		}
		signerLog.WarnContext(ctx, msg, "login", row.Login, "host", row.Host, "retry_in", d.backOff(), "error", err)
		return
	}
	check := db.DeploymentSignerKeyCheck{KeyFound: read.keyFound}
	if read.found {
		check.GitHubUserID, check.Login, check.Name = read.id, read.account.Login, read.account.Name
	}
	result := domain.CommitVerificationValid
	if !read.keyFound {
		result = CheckResultKeyMissing
	}
	CountCheck(ctx, CheckPeriodic, result)
	switch {
	case !read.found && read.account.Login != "":
		signerLog.WarnContext(ctx, "the deployment commit signer's login names an account that is not a user; a signer must be a user account",
			"login", read.account.Login, "type", read.account.Type, "host", row.Host)
	case !read.found && row.GitHubUserID > 0:
		signerLog.WarnContext(ctx, "GitHub no longer has the account the deployment commit signer's key signs as; "+newKeyAdvice,
			"login", row.Login, "github_user_id", row.GitHubUserID, "host", row.Host)
	case !read.found:
		signerLog.WarnContext(ctx, "GitHub has no account for the deployment commit signer",
			"login", row.Login, "host", row.Host)
	case !read.keyFound:
		signerLog.WarnContext(ctx, "the deployment commit signer's key is not among the account's signing keys; add it under Settings → SSH and GPG keys with key type Signing key",
			"login", read.account.Login, "host", row.Host, "public_key", d.Signer.PublicKey)
	}
	if d.loginMoved(row, read) {
		signerLog.WarnContext(ctx, EnvDeploymentSignerLogin+" names another account than the one the deployment commit signer's key signs as; after a rename, set it to the account's login, and "+newKeyAdvice,
			"configured_login", d.Signer.Login, "github_user_id", row.GitHubUserID, "login", read.account.Login, "host", row.Host)
	}
	updated, err := d.Stores.DeploymentCommitSigners.RecordKeyCheckSystem(ctx, row, check)
	if err != nil {
		signerLog.ErrorContext(ctx, "deployment commit signer periodic check could not be recorded; this pod tries again after retry_in",
			"retry_in", d.backOff(), "error", err)
		return
	}
	d.failures, d.retryAt = 0, time.Time{}
	if updated != nil {
		d.observe(ctx, *updated)
	}
}

// VerifyFault is a Verify that did not pass: Message says which check failed
// and what to do about it, for the operator.
type VerifyFault struct {
	Message string
}

// Verify checks the signer against GitHub now and, when it passes, makes it
// active: its key is among the account's signing keys and, for a signer a
// pushed commit broke, that commit verifies now. GitHub evaluates a failing
// result again on every read, so the commit reads valid once its fault is
// fixed; one signed with another key, committed under an address the signer
// no longer commits as, or gone from GitHub, holds the signer no longer
// (DeploymentFailureHolds), and the next push is checked instead.
//
// It returns the stored row, or the fault a check that did not pass answers
// with; err is a failure of TF's own, which is neither.
func (d *DeploymentHealth) Verify(ctx context.Context) (domain.DeploymentCommitSigner, *VerifyFault, error) {
	row, err := d.Stores.DeploymentCommitSigners.RegisterSystem(ctx, d.Signer.Host, d.Signer.PublicKey, d.Signer.Login)
	if err != nil {
		return domain.DeploymentCommitSigner{}, nil, fmt.Errorf("read the deployment commit signer: %w", err)
	}
	result, fault, err := d.check(ctx, row)
	if err != nil {
		return domain.DeploymentCommitSigner{}, nil, err
	}
	CountCheck(ctx, CheckVerify, result.reason)
	if fault != nil {
		return row, fault, nil
	}
	updated, err := d.Stores.DeploymentCommitSigners.ActivateSystem(ctx, d.Signer.Host, d.Signer.PublicKey,
		result.accountID, result.account.Login, result.account.Name)
	if err != nil {
		return domain.DeploymentCommitSigner{}, nil, fmt.Errorf("activate the deployment commit signer: %w", err)
	}
	d.observe(ctx, updated)
	return updated, nil, nil
}

// verifyResult is a Verify's reading of GitHub: the reason
// tf_commit_signer_checks_total counts it under and, for one that passed, the
// account it read.
type verifyResult struct {
	reason    string
	account   ghclient.Account
	accountID int64
}

// check is Verify's reading of GitHub, without the write it leads to.
func (d *DeploymentHealth) check(ctx context.Context, row domain.DeploymentCommitSigner) (verifyResult, *VerifyFault, error) {
	read, err := d.readKey(ctx, row)
	if err != nil {
		if ghclient.IsHTTP404(err) {
			return verifyResult{reason: CheckResultError}, &VerifyFault{Message: d.Signer.Host + " does not serve SSH signing keys, so it cannot verify a commit TF signs"}, nil
		}
		return verifyResult{reason: CheckResultError}, &VerifyFault{Message: "could not read the deployment signer's account from GitHub: " + err.Error()}, nil
	}
	missing := verifyResult{reason: CheckResultKeyMissing}
	switch {
	case !read.found && read.account.Login != "":
		return missing, &VerifyFault{Message: fmt.Sprintf("%s is a %s account on %s; the deployment signer must be a user account. Set %s to one",
			read.account.Login, read.account.Type, d.Signer.Host, EnvDeploymentSignerLogin)}, nil
	case !read.found && row.GitHubUserID > 0:
		return missing, &VerifyFault{Message: fmt.Sprintf("%s no longer has account %d (last seen as %s), the account this key signs as; %s",
			d.Signer.Host, row.GitHubUserID, row.Login, newKeyAdvice)}, nil
	case !read.found:
		return missing, &VerifyFault{Message: fmt.Sprintf("%s has no account %s; set %s to the account that should sign", d.Signer.Host, row.Login, EnvDeploymentSignerLogin)}, nil
	case !read.keyFound && d.loginMoved(row, read):
		return missing, &VerifyFault{Message: fmt.Sprintf("%s is %s, but this key signs as account %d, now %s, and is not among its signing keys. To keep signing as %s, put the key back on it and set %s to %s; %s",
			EnvDeploymentSignerLogin, d.Signer.Login, row.GitHubUserID, read.account.Login, read.account.Login, EnvDeploymentSignerLogin, read.account.Login, newKeyAdvice)}, nil
	case !read.keyFound:
		return missing, &VerifyFault{Message: fmt.Sprintf("the deployment signer's key is not among %s's signing keys on GitHub; add it in Settings → SSH and GPG keys → New SSH key with key type Signing key, then verify again. GitHub's listing can take a minute to show a key just added",
			read.account.Login)}, nil
	}
	passed := verifyResult{reason: domain.CommitVerificationValid, account: read.account, accountID: read.id}
	if row.Status != domain.CommitSignerBroken || row.StatusReason == "" || row.StatusReason == domain.CommitSignerReasonKeyMissing {
		return passed, nil, nil
	}
	reason, fault, err := d.recheckFailedCommit(ctx, row, read.id, read.account.Login)
	if err != nil || fault != nil {
		return verifyResult{reason: reason}, fault, err
	}
	return passed, nil, nil
}

// recheckFailedCommit re-reads the newest pushed commit, in any org, that
// GitHub reported as not verified for the signer's account.
func (d *DeploymentHealth) recheckFailedCommit(ctx context.Context, row domain.DeploymentCommitSigner, accountID int64, login string) (string, *VerifyFault, error) {
	failed, err := d.Stores.DeploymentCommitSigners.NewestFailedVerificationSystem(ctx, d.Signer.Host, accountID)
	if err != nil {
		return "", nil, fmt.Errorf("read the deployment signer's failing commit: %w", err)
	}
	if failed == nil || failed.PublicKey != d.Signer.PublicKey || failed.CommitSHA == "" || failed.RepositoryID == "" {
		return domain.CommitVerificationValid, nil, nil
	}
	repo, err := d.Stores.Repos.GetSystem(ctx, failed.OrgID, failed.RepositoryID)
	if err != nil {
		return "", nil, fmt.Errorf("read the failing commit's repository: %w", err)
	}
	if repo == nil {
		return domain.CommitVerificationValid, nil, nil
	}
	client, err := ghclient.ClientForOwner(ctx, d.GitHub, failed.OrgID, repo.OwnerExternalID, repo.Owner)
	if err != nil {
		return CheckResultError, &VerifyFault{Message: "could not reach " + repo.Slug() + " on GitHub: " + err.Error()}, nil
	}
	verdict, err := client.GetCommitVerification(ctx, repo.Owner, repo.Repo, failed.CommitSHA)
	if ghclient.IsHTTP404(err) {
		return domain.CommitVerificationValid, nil, nil
	}
	if err != nil {
		return CheckResultError, &VerifyFault{Message: "could not re-read commit " + failed.CommitSHA + " from GitHub: " + err.Error()}, nil
	}
	if VerificationUnavailable(verdict.Reason) {
		return CheckResultError, &VerifyFault{Message: "GitHub's signature verification did not answer for commit " + failed.CommitSHA + " (" + verdict.Reason + "); verify again shortly"}, nil
	}
	current := row
	current.GitHubUserID = accountID
	if !DeploymentFailureHolds(d.Signer, current, *failed, verdict) {
		return domain.CommitVerificationValid, nil, nil
	}
	return verdict.Reason, &VerifyFault{Message: fmt.Sprintf("GitHub still reports commit %s in %s as not verified (%s): %s",
		failed.CommitSHA, repo.Slug(), verdict.Reason, UnverifiedCommitAdvice(verdict.Reason, verdict.CommitterEmail, login))}, nil
}

// DeploymentFailureHolds is FailureHolds for the deployment signer as row
// describes it: a failing pushed commit counts against it only when it was
// signed as its account with its key, and committed under the address it
// commits as now.
func DeploymentFailureHolds(d *DeploymentSigner, row domain.DeploymentCommitSigner, sig domain.CommitSignature, verdict ghclient.CommitVerification) bool {
	return failureHolds(row.Host, row.GitHubUserID, d.CommitEmail(row), d.PublicKey, sig, verdict)
}

// UnverifiedCommitAdvice says what to do about the verification reasons that
// are fixed on the signer's account.
func UnverifiedCommitAdvice(reason, email, login string) string {
	switch reason {
	case domain.CommitVerificationUnknownKey:
		return "the key that signed it is not on " + login + "'s signing keys"
	case "no_user":
		return email + " belongs to no GitHub account; add and verify it on " + login + ", or change the signer's email"
	case "unverified_email":
		return email + " is not verified on " + login + "; verify it there, then verify the signer again"
	default:
		return "GitHub gave this reason for the commit's signature; fix it on " + login + ", then verify again"
	}
}

// SetGitHubForTest points the signer's account reads at baseURL for the
// duration of t, so a test outside this package can serve them from a fake.
// It lives in a non-_test.go file for that reason.
func (d *DeploymentHealth) SetGitHubForTest(t TestT, baseURL string) {
	t.Helper()
	prev := d.client
	d.client = ghclient.NewClient(baseURL, "")
	t.Cleanup(func() { d.client = prev })
}
