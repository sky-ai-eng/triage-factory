package server

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The deployment commit signer's routes: the account every workspace on the
// deployment App signs its agents' commits as unless it registers its own,
// whose login and key are control-pod config. Their subject is the
// deployment, so they are gated on the deployment operator flag and no API
// token reaches them. They are core and not part of the licensed fleet
// console: commit signing is core, and Verify is the only way out of a signer
// a pushed commit broke.
//
// Each answers for the signer this control pod is configured with. Every
// control pod should carry the same TF_GITHUB_COMMIT_SIGNER_* settings; during
// a rolling change of them, two pods can answer for different keys.

// SetDeploymentCommitSigner gives the server this control pod's deployment
// signer, nil when none is configured: the routes in this file answer for
// it, and GET commit-identity resolves a managed workspace to it.
func (s *Server) SetDeploymentCommitSigner(h *commitsigner.DeploymentHealth) {
	s.deploymentSigner = h
}

// commitIdentityResolver is the resolution a run's commits take, with the
// deployment arm when this pod has a deployment signer.
func (s *Server) commitIdentityResolver() commitsigner.Resolver {
	r := commitsigner.Resolver{Signers: s.allStores.OrgCommitSigners, GitHub: s.ghResolver}
	if s.deploymentSigner != nil {
		r.Deployment = &commitsigner.DeploymentArm{
			Signer: s.deploymentSigner.Signer, Health: s.allStores.DeploymentCommitSigners, Orgs: s.allStores.Orgs,
		}
	}
	return r
}

// requireOperator admits a deployment operator signed in with a session. A
// caller who is not an operator gets a 404, so the routes do not exist for
// them; an operator holding an API token gets a 403, because a token is
// sealed to one org and the console's subject is the deployment. It reports
// whether the caller was admitted, having written the response when not.
func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request) bool {
	claims := httpx.ClaimsFrom(r.Context())
	if claims == nil {
		httpx.WriteUnauth(w)
		return false
	}
	ok, err := s.isDeploymentOperator(r.Context(), claims.Subject)
	if err != nil {
		internalError(w, "fleet-commit-signer", err)
		return false
	}
	if !ok {
		notFound(w, "fleet")
		return false
	}
	if httpx.TokenAuthFrom(r.Context()) != nil {
		httpx.WriteErrors(w, http.StatusForbidden, httpx.ErrorItem{
			Reason:  httpx.ReasonForbidden,
			Message: "the deployment commit signer is deployment-scoped and cannot be reached with an org-scoped API token; sign in to use it",
		})
		return false
	}
	return true
}

// isDeploymentOperator is the operator read, the same one the fleet console's
// gate makes: whether a grant is bound to the principal, or one of its login
// identities holds a pending grant's address verified.
func (s *Server) isDeploymentOperator(ctx context.Context, userID string) (bool, error) {
	if s.allStores.Operators == nil {
		return false, nil
	}
	return s.allStores.Operators.IsOperatorUserSystem(ctx, userID)
}

// requireDeploymentSigner answers 404 for a control pod with no deployment
// signer.
func (s *Server) requireDeploymentSigner(w http.ResponseWriter) (*commitsigner.DeploymentHealth, bool) {
	if s.deploymentSigner == nil {
		notFound(w, "deployment commit signer")
		return nil, false
	}
	return s.deploymentSigner, true
}

// deploymentSignerJSON is the deployment signer as these routes answer it.
type deploymentSignerJSON struct {
	// Host is the GitHub the account is on, the deployment App's.
	Host string `json:"host"`
	// Login is the account's login as the last check read it, and the
	// configured login until a check has. ConfiguredLogin is
	// TF_GITHUB_COMMIT_SIGNER_LOGIN.
	Login           string `json:"login"`
	ConfiguredLogin string `json:"configured_login"`
	// GitHubUserID is the account the login named when a check first read
	// it, null until one has: until then no workspace commits as it.
	GitHubUserID *string `json:"github_user_id"`
	Name         *string `json:"name"`
	// Email is the configured committer address, null for the derived
	// noreply address. CommitName and CommitEmail are what the signer's
	// commits carry, null until the account is known.
	Email       *string `json:"email"`
	CommitName  *string `json:"commit_name"`
	CommitEmail *string `json:"commit_email"`
	// PublicKey is the configured key's public half, the one the account
	// must list as a signing key.
	PublicKey    string  `json:"public_key"`
	Status       string  `json:"status"`
	StatusReason *string `json:"status_reason"`
	// StatusSince is null before this pod has registered the key, which it
	// does as it starts.
	StatusSince *time.Time `json:"status_since"`
	CheckedAt   *time.Time `json:"checked_at"`
}

func toDeploymentSignerJSON(d *commitsigner.DeploymentSigner, row domain.DeploymentCommitSigner) deploymentSignerJSON {
	out := deploymentSignerJSON{
		Host:            row.Host,
		Login:           row.Login,
		ConfiguredLogin: d.Login,
		Name:            nilIfEmpty(row.Name),
		Email:           nilIfEmpty(d.EmailOverride),
		PublicKey:       row.PublicKey,
		Status:          string(row.Status),
		StatusReason:    nilIfEmpty(row.StatusReason),
		CheckedAt:       row.CheckedAt,
	}
	if !row.StatusSince.IsZero() {
		since := row.StatusSince
		out.StatusSince = &since
	}
	if row.GitHubUserID > 0 {
		out.GitHubUserID = nilIfEmpty(strconv.FormatInt(row.GitHubUserID, 10))
		out.CommitName = nilIfEmpty(row.CommitName())
		out.CommitEmail = nilIfEmpty(d.CommitEmail(row))
	}
	return out
}

// handleDeploymentSignerGet answers the deployment signer this control pod is
// configured with and its health, 404 when it has none.
//
// GET /api/fleet/commit-signer
func (s *Server) handleDeploymentSignerGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r) {
		return
	}
	h, ok := s.requireDeploymentSigner(w)
	if !ok {
		return
	}
	row, err := s.allStores.DeploymentCommitSigners.GetSystem(r.Context(), h.Signer.Host, h.Signer.PublicKey)
	if err != nil {
		internalError(w, "fleet-commit-signer", err)
		return
	}
	if row == nil {
		row = &domain.DeploymentCommitSigner{Host: h.Signer.Host, PublicKey: h.Signer.PublicKey, Login: h.Signer.Login, Status: domain.CommitSignerPending}
	}
	writeJSON(w, http.StatusOK, toDeploymentSignerJSON(h.Signer, *row))
}

// handleDeploymentSignerVerify checks the deployment signer against GitHub now
// and, when it passes, makes it active: its key is among the account's signing
// keys and, for a signer a pushed commit broke, that commit verifies now.
//
// POST /api/fleet/commit-signer/verify
func (s *Server) handleDeploymentSignerVerify(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r) {
		return
	}
	h, ok := s.requireDeploymentSigner(w)
	if !ok {
		return
	}
	row, fault, err := h.Verify(r.Context())
	if err != nil {
		internalError(w, "fleet-commit-signer", err)
		return
	}
	if fault != nil {
		status, reason := http.StatusConflict, httpx.ReasonKeyNotFound
		switch fault.Kind {
		case commitsigner.FaultSSHSigningUnsupported:
			status, reason = http.StatusUnprocessableEntity, httpx.ReasonSSHSigningUnsupported
		case commitsigner.FaultCommitUnverified:
			reason = httpx.ReasonCommitUnverified
		case commitsigner.FaultUpstream:
			status, reason = http.StatusBadGateway, httpx.ReasonUpstreamUnavailable
		}
		httpx.WriteErrors(w, status, httpx.ErrorItem{Reason: reason, Message: fault.Message})
		return
	}
	writeJSON(w, http.StatusOK, toDeploymentSignerJSON(h.Signer, row))
}

// deploymentSignerUsageJSON is the deployment signer's usage node: the
// signatures made as it in the window, by hour and by org. Its total is the
// sum of its orgs'; the signer has no budget of its own.
type deploymentSignerUsageJSON struct {
	Since  *time.Time                 `json:"since"`
	Until  *time.Time                 `json:"until"`
	Total  int                        `json:"total"`
	ByHour []deploymentSignerHourJSON `json:"by_hour"`
	ByOrg  []deploymentSignerOrgJSON  `json:"by_org"`
}

type deploymentSignerHourJSON struct {
	// Hour is the hour's start, UTC.
	Hour       time.Time `json:"hour"`
	Signatures int       `json:"signatures"`
}

type deploymentSignerOrgJSON struct {
	OrgID string `json:"org_id"`
	// Name is the org's name, null for an org deleted since.
	Name       *string `json:"name"`
	Signatures int     `json:"signatures"`
}

// handleDeploymentSignerUsage counts the signatures made as the deployment
// signer in [since, until), every org's: hours with none and orgs with none
// are left out. Both bounds absent is the current month to now; one absent
// leaves that side open.
//
// GET /api/fleet/commit-signer/usage?since=&until=
func (s *Server) handleDeploymentSignerUsage(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r) {
		return
	}
	if _, ok := s.requireDeploymentSigner(w); !ok {
		return
	}
	since, until, errMsg := parseUsageWindow(r.URL.Query(), time.Now().UTC())
	if errMsg != "" {
		badRequest(w, errMsg)
		return
	}
	usage, err := s.allStores.DeploymentCommitSigners.UsageSystem(r.Context(), since, until)
	if err != nil {
		internalError(w, "fleet-commit-signer-usage", err)
		return
	}
	out := deploymentSignerUsageJSON{
		Total:  usage.Total,
		ByHour: make([]deploymentSignerHourJSON, 0, len(usage.ByHour)),
		ByOrg:  make([]deploymentSignerOrgJSON, 0, len(usage.ByOrg)),
	}
	if !since.IsZero() {
		out.Since = &since
	}
	if !until.IsZero() {
		out.Until = &until
	}
	for _, h := range usage.ByHour {
		out.ByHour = append(out.ByHour, deploymentSignerHourJSON{Hour: h.Hour, Signatures: h.Count})
	}
	for _, o := range usage.ByOrg {
		row := deploymentSignerOrgJSON{OrgID: o.OrgID, Signatures: o.Count}
		org, err := s.allStores.Orgs.GetOrgSystem(r.Context(), o.OrgID)
		if err != nil {
			internalError(w, "fleet-commit-signer-usage", err)
			return
		}
		if org != nil {
			row.Name = nilIfEmpty(org.Name)
		}
		out.ByOrg = append(out.ByOrg, row)
	}
	writeJSON(w, http.StatusOK, out)
}
