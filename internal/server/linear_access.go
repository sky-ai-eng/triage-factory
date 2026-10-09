package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/linearoauth"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The org's Linear service credential as a first-class resource: the bind
// (PUT) and unbind (DELETE) pair whose rationale sits with the GitHub and Jira
// routes in org_credentials.go, plus the status read the settings card and the
// setup wizard render from.
//
//   PUT    /api/orgs/{org_id}/linear/access/credential
//   DELETE /api/orgs/{org_id}/linear/access/credential
//   GET    /api/orgs/{org_id}/linear/access
//
// The bind takes one shape, a personal API key. Linear has one host, so the
// body carries no URL and no deployment discriminator, and the workspace is
// learned from the key rather than typed: the bind writes
// org_settings.linear_workspace_id / linear_workspace_url_key from the key's
// organization, and the unbind clears them. The other shape, an installed app,
// is bound by the install ceremony (linear_install.go) and unbound here.

// linearCredentialRequest is the PUT body: the key, and nothing else.
type linearCredentialRequest struct {
	APIKey string `json:"api_key"`
}

// linearBinding is who the org's Linear credential validated as, and the name
// of the workspace it belongs to, as stored under integrations.KeyLinearBoundAs
// at bind time. Nothing in it is secret; it lives beside the credential so it
// is written and cleared with it.
type linearBinding struct {
	Name          string `json:"name"`
	DisplayName   string `json:"display_name"`
	WorkspaceName string `json:"workspace_name,omitempty"`
}

// linearBoundAsJSON is the viewer half of linearBinding on the wire.
type linearBoundAsJSON struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// linearAccessStatus is the GET body, and what a successful bind answers with.
//
// connect_available is whether a Linear OAuth app resolves for the org, so the
// install ceremony can run; using_deployment_default is whether that app is
// the deployment's rather than the org's own. last_error explains a
// disconnection the org did not ask for: "install_revoked" when the app was
// removed from the workspace in Linear and its refresh token refused.
type linearAccessStatus struct {
	Connected              bool               `json:"connected"`
	AuthMethod             string             `json:"auth_method"`
	WorkspaceURLKey        string             `json:"workspace_url_key"`
	WorkspaceName          string             `json:"workspace_name,omitempty"`
	BoundAs                *linearBoundAsJSON `json:"bound_as"`
	ConnectAvailable       bool               `json:"connect_available"`
	UsingDeploymentDefault bool               `json:"using_deployment_default"`
	LastError              string             `json:"last_error,omitempty"`
}

// errLinearAppInstalled is the bind finding the org's Linear credential is a
// live installed app. Binding a key over it would leave the install's tokens
// live in Linear with nothing in TF left to revoke them, so the bind refuses
// and the admin disconnects first. An install Linear already revoked has no
// tokens left, and a key may replace it.
var errLinearAppInstalled = errors.New("linear: org credential is an installed app")

// handleLinearCredentialPut binds (or rotates) the org's Linear API key. It
// validates the key live, then in one transaction stores it under the api_key
// marker, drops any app-install envelope, records who it validated as, writes
// the workspace columns, and records the access-log row. Org-admin only.
//
// Deliberately NOT bound to the caller's own Linear identity: per-user access
// is its own surface, so org access and user identity stay independent even
// when the same key serves both.
//
// PUT /api/orgs/{org_id}/linear/access/credential
func (s *Server) handleLinearCredentialPut(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	var req linearCredentialRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	key := strings.TrimSpace(req.APIKey)
	if key == "" {
		httpx.WriteErrors(w, http.StatusBadRequest, httpx.ErrorItem{Reason: httpx.ReasonMissingField, Message: "A Linear API key is required.", Field: "api_key"})
		return
	}

	viewer, org, err := s.validateLinear(ctx, linear.APIKey(key))
	switch {
	case errors.Is(err, linear.ErrUnauthorized):
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "Linear rejected this API key", Field: "api_key"})
		return
	case err != nil:
		writeLinearUpstream(w, orgID, "who this API key belongs to", err)
		return
	case viewer.IsApp:
		// A personal key never acts as an app user. Refusing one that does
		// keeps "an api_key credential is a person's" true by construction
		// rather than by Linear's say-so.
		httpx.WriteErrors(w, http.StatusUnprocessableEntity, httpx.ErrorItem{Reason: httpx.ReasonUpstreamRejected, Message: "this key acts as an app user, not a person — paste a personal API key", Field: "api_key"})
		return
	}

	binding, err := json.Marshal(linearBinding{Name: viewer.Name, DisplayName: viewer.DisplayName, WorkspaceName: org.Name})
	if err != nil {
		internalError(w, "linear-access", fmt.Errorf("marshal linear binding: %w", err))
		return
	}

	// Held across the install check and the write, so an install completing
	// on another pod cannot land between them.
	release, err := s.lockLinearCredential(ctx, orgID)
	if err != nil {
		internalError(w, "linear-access", err)
		return
	}
	defer release()

	// A failure later in the transaction puts the prior keys back rather than
	// leaving the new key half-bound — or, on a rotation, deleting the key
	// that was working.
	restore, unlock, err := s.guardLocalLinearWrite(ctx, orgID)
	if err != nil {
		internalError(w, "linear-access", err)
		return
	}
	defer unlock()

	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		method, err := tx.Secrets.Get(ctx, orgID, integrations.KeyLinearAuthMethod)
		if err != nil {
			return fmt.Errorf("read linear auth method: %w", err)
		}
		if linear.AuthMethod(method) == linear.AuthMethodAppInstall {
			envelope, err := tx.Secrets.Get(ctx, orgID, integrations.KeyLinearAppInstall)
			if err != nil {
				return fmt.Errorf("read linear install credential: %w", err)
			}
			if envelope != "" {
				return errLinearAppInstalled
			}
		}
		// The keys are written in the order ClearLinear deletes them, the
		// unbind's order. Postgres holds each row's lock until commit, so two
		// writers taking the same rows in different orders can deadlock, and
		// one of them fails.
		for _, kv := range [][2]string{
			{integrations.KeyLinearAPIKey, key},
			{integrations.KeyLinearAuthMethod, string(linear.AuthMethodAPIKey)},
		} {
			if err := tx.Secrets.Put(ctx, orgID, kv[0], kv[1], ""); err != nil {
				return fmt.Errorf("store %s: %w", kv[0], err)
			}
		}
		if err := integrations.ClearLinearOtherScheme(ctx, tx.Secrets, orgID, linear.AuthMethodAPIKey); err != nil {
			return fmt.Errorf("clear stale linear credential: %w", err)
		}
		if err := tx.Secrets.Put(ctx, orgID, integrations.KeyLinearBoundAs, string(binding), ""); err != nil {
			return fmt.Errorf("store %s: %w", integrations.KeyLinearBoundAs, err)
		}
		// TODO(TFAC-1060): a key from a different workspace is accepted here.
		// The tracker then matches the new workspace's issues to the old rows
		// by identifier alone, overwriting their text and giving them the old
		// rows' tasks and history, and every team's Linear team rules keep
		// naming the old workspace's teams and states. Scoping entities and
		// team rules by workspace closes both.
		if _, err := tx.Orgs.SetLinearWorkspace(ctx, orgID, org.ID, org.URLKey); err != nil {
			return fmt.Errorf("save linear workspace: %w", err)
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialSet,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindLinearOrg, "", org.URLKey),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		if errors.Is(err, errLinearAppInstalled) {
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this workspace's Linear access is an installed app — disconnect the installed app first", Field: "api_key"})
			return
		}
		internalError(w, "linear-access", fmt.Errorf("persist linear credential: %w", err))
		return
	}

	s.kickLinearChanged(r, orgID)

	status, err := s.readLinearAccess(ctx, orgID, userID)
	if err != nil {
		internalError(w, "linear-access", err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleLinearCredentialDelete unbinds the org's Linear credential — the
// disconnect. It clears the credential, its bound-as record and the workspace
// columns in one transaction. Idempotent: an org with nothing bound gets a 200
// and no audit row, because a removal that removed nothing is not an access
// change.
//
// It is an installed app's disconnect too. Once the clear has committed, the
// org's live install row is marked removed, which releases its workspace, and
// the install's refresh token is revoked in Linear. The row is released only
// after the commit, so a clear that fails never frees a workspace the org is
// still polling; a release that fails answers 500, and the retry releases it,
// since a live row with no credential behind it is released whatever the
// credential was. The revoke is best-effort: TF has already forgotten the
// token, so a failure only logs. The app user stays in the workspace's member
// list until a workspace admin removes the app in Linear.
//
// Per-user Linear credentials are left intact: each is custodied under its
// owner's own secret scope and cleared only by its own surface.
//
// DELETE /api/orgs/{org_id}/linear/access/credential
func (s *Server) handleLinearCredentialDelete(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	release, err := s.lockLinearCredential(ctx, orgID)
	if err != nil {
		internalError(w, "linear-access", err)
		return
	}
	defer release()

	// A failure after the clear puts the credential back: the request reports
	// the disconnect failed, so the org must still be connected.
	restore, unlock, err := s.guardLocalLinearWrite(ctx, orgID)
	if err != nil {
		internalError(w, "linear-access", err)
		return
	}
	defer unlock()

	var (
		had      bool
		envelope string
	)
	if err := s.tx.WithTx(ctx, orgID, userID, func(tx db.TxStores) error {
		method, err := tx.Secrets.Get(ctx, orgID, integrations.KeyLinearAuthMethod)
		if err != nil {
			return fmt.Errorf("read linear auth method: %w", err)
		}
		key, err := tx.Secrets.Get(ctx, orgID, integrations.KeyLinearAPIKey)
		if err != nil {
			return fmt.Errorf("read linear api key: %w", err)
		}
		had = method != "" || key != ""
		if linear.AuthMethod(method) == linear.AuthMethodAppInstall {
			if envelope, err = tx.Secrets.Get(ctx, orgID, integrations.KeyLinearAppInstall); err != nil {
				return fmt.Errorf("read linear install credential: %w", err)
			}
		}
		if err := integrations.ClearLinear(ctx, tx.Secrets, orgID); err != nil {
			return fmt.Errorf("clear credential: %w", err)
		}
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		prevWorkspace := orgSet.LinearWorkspaceURLKey
		if orgSet.LinearWorkspaceID != "" || prevWorkspace != "" {
			if _, err := tx.Orgs.SetLinearWorkspace(ctx, orgID, "", ""); err != nil {
				return fmt.Errorf("clear linear workspace: %w", err)
			}
		}
		if !had {
			return nil
		}
		return tx.AccessChangeLog.Record(ctx, orgID, domain.AccessChange{
			ActorUserID: userID,
			Action:      domain.AccessActionCredentialRemoved,
			DetailJSON:  accessDetailCredentialNamed(domain.CredentialKindLinearOrg, "", prevWorkspace),
		})
	}); err != nil {
		if restore != nil {
			restore()
		}
		internalError(w, "linear-access", err)
		return
	}

	// Nothing was polling under a credential that was not there.
	if had {
		s.kickLinearChanged(r, orgID)
	}
	// The release reads the live row and marks it by its install id, so it
	// runs under the lock, where no install can replace that row between the
	// two. The revoke is network I/O and runs after it, whether or not the
	// release succeeded: the credential is already gone, and a retry would
	// find no token left to revoke.
	releaseErr := s.releaseLinearInstall(ctx, orgID)
	release()
	if envelope != "" {
		s.revokeLinearInstall(ctx, orgID, envelope)
	}
	if releaseErr != nil {
		internalError(w, "linear-access", releaseErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// releaseLinearInstall marks the org's live install row removed as
// disconnected, freeing its workspace. The caller has just cleared the org's
// credential under the Linear credential lock, so no live row can have a
// credential behind it.
func (s *Server) releaseLinearInstall(ctx context.Context, orgID string) error {
	inst, err := s.liveLinearInstall(ctx, orgID)
	if err != nil {
		return fmt.Errorf("read linear install: %w", err)
	}
	if inst == nil {
		return nil
	}
	if _, err := s.linearInstalls.MarkRemovedSystem(ctx, orgID, inst.InstallID, domain.LinearInstallRemovedDisconnected); err != nil {
		return fmt.Errorf("release linear install: %w", err)
	}
	return nil
}

// revokeLinearInstall revokes a disconnected install's refresh token in
// Linear, with the app that minted it. Best-effort: the credential is already
// gone from TF, so a failure leaves only a grant nothing uses, which a
// workspace admin ends by removing the app in Linear.
func (s *Server) revokeLinearInstall(ctx context.Context, orgID, envelope string) {
	ctx = context.WithoutCancel(ctx)
	cred, err := linear.ParseInstallCredential(envelope)
	if err != nil {
		linearInstallLog.Warn("disconnected install credential unreadable; nothing to revoke", "org", orgID, "error", err)
		return
	}
	app, _, err := s.linearOAuthApps.Resolve(ctx, orgID)
	if err != nil || app.ClientID != cred.ClientID {
		linearInstallLog.Warn("the app that minted the disconnected install no longer resolves; its grant stays until the app is removed in Linear",
			"org", orgID, "client_id", cred.ClientID, "error", err)
		return
	}
	if err := s.linearOAuthMinter.Revoke(ctx, app, cred.RefreshToken, linearoauth.HintRefreshToken); err != nil {
		linearInstallLog.Warn("revoke disconnected install failed; its grant stays until the app is removed in Linear", "org", orgID, "error", err)
		return
	}
	linearInstallLog.Info("revoked disconnected install", "org", orgID, "workspace", cred.WorkspaceID)
}

// handleLinearAccessGet reports the org's Linear connection. Any org member
// reads it: the settings card and the setup wizard render for everyone, and
// nothing in it is secret.
//
// GET /api/orgs/{org_id}/linear/access
func (s *Server) handleLinearAccessGet(w http.ResponseWriter, r *http.Request) {
	orgID, userID, ok := s.az.RequireOrgMember(w, r)
	if !ok {
		return
	}
	status, err := s.readLinearAccess(r.Context(), orgID, userID)
	if err != nil {
		internalError(w, "linear-access", err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// readLinearAccess derives the status body. connected is the same derivation
// the event-source probe reads (integrations.LinearSystemConfigured), so the
// card and Settings → Event sources cannot disagree. A stored key with no
// auth-method marker reads as api_key, the default the resolver applies.
func (s *Server) readLinearAccess(ctx context.Context, orgID, userID string) (linearAccessStatus, error) {
	var (
		creds   auth.Credentials
		orgSet  domain.OrgSettings
		binding string
		install *domain.OrgLinearInstall
	)
	if err := s.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		var err error
		if creds, err = integrations.Load(ctx, tx.Secrets, orgID); err != nil {
			return fmt.Errorf("load credentials: %w", err)
		}
		if orgSet, err = tx.Orgs.GetSettings(ctx, orgID); err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		if binding, err = tx.Secrets.Get(ctx, orgID, integrations.KeyLinearBoundAs); err != nil {
			return fmt.Errorf("read linear binding: %w", err)
		}
		if install, err = tx.LinearInstalls.GetForOrgSystem(ctx, orgID); err != nil {
			return fmt.Errorf("read linear install: %w", err)
		}
		return nil
	}); err != nil {
		return linearAccessStatus{}, err
	}

	source := s.resolveLinearAppSource(ctx, orgID)
	status := linearAccessStatus{
		Connected:              integrations.LinearSystemConfigured(creds),
		ConnectAvailable:       source != linear.SourceNone,
		UsingDeploymentDefault: source == linear.SourceDeployment,
	}
	if !status.Connected {
		// An app_install marker left behind with no envelope is an install
		// whose refresh Linear refused; the row says why.
		if linear.AuthMethod(creds.LinearAuthMethod) == linear.AuthMethodAppInstall &&
			install != nil && install.RemovedReason == domain.LinearInstallRemovedRevoked {
			status.LastError = domain.LinearInstallRemovedRevoked
		}
		return status, nil
	}
	status.AuthMethod = creds.LinearAuthMethod
	if status.AuthMethod == "" {
		status.AuthMethod = string(linear.AuthMethodAPIKey)
	}
	status.WorkspaceURLKey = orgSet.LinearWorkspaceURLKey
	if binding != "" {
		var b linearBinding
		if err := json.Unmarshal([]byte(binding), &b); err != nil {
			// Display-only: a record that will not parse costs the card its
			// name, not the read.
			serverLog.Warn("linear binding unreadable", "org", orgID, "error", err)
		} else {
			status.WorkspaceName = b.WorkspaceName
			status.BoundAs = &linearBoundAsJSON{Name: b.Name, DisplayName: b.DisplayName}
		}
	}
	return status, nil
}

// kickLinearChanged re-dues the org's Linear poll under a changed credential
// and clears its readiness until that poll completes — the Linear sibling of
// kickJiraChanged.
func (s *Server) kickLinearChanged(r *http.Request, orgID string) {
	if s.onLinearChanged == nil {
		return
	}
	s.MarkLinearRestarted(r.Context(), orgID)
	go s.onLinearChanged(orgID)
}
