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
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
// organization, and the unbind clears them.

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
// connect_available and using_deployment_default describe the app-install
// shape, which this build cannot bind; they are false here and present so the
// client contract does not change when it can.
type linearAccessStatus struct {
	Connected              bool               `json:"connected"`
	AuthMethod             string             `json:"auth_method"`
	WorkspaceURLKey        string             `json:"workspace_url_key"`
	WorkspaceName          string             `json:"workspace_name,omitempty"`
	BoundAs                *linearBoundAsJSON `json:"bound_as"`
	ConnectAvailable       bool               `json:"connect_available"`
	UsingDeploymentDefault bool               `json:"using_deployment_default"`
}

// errLinearAppInstalled is the bind finding the org's Linear credential is an
// installed app. Binding a key over it would leave the install's tokens live
// in Linear with nothing in TF left to revoke them, so the bind refuses and
// the admin disconnects first.
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
			return errLinearAppInstalled
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
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		// TODO(TFAC-1060): a key from a different workspace is accepted here,
		// and the tracker then matches the new workspace's issues to the old
		// rows by identifier alone, overwriting their text and giving them the
		// old rows' tasks and history. Matching on the issue UUID, plus an
		// explicit answer for a workspace switch, closes it.
		orgSet.LinearWorkspaceID = org.ID
		orgSet.LinearWorkspaceURLKey = org.URLKey
		if err := saveLinearWorkspace(ctx, tx, orgID, orgSet); err != nil {
			return err
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
		switch {
		case errors.Is(err, errLinearAppInstalled):
			httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonConflict, Message: "this workspace's Linear access is an installed app — disconnect the installed app first", Field: "api_key"})
		case errors.Is(err, db.ErrOrgSettingsVersion):
			writeLinearSettingsRace(w)
		default:
			internalError(w, "linear-access", fmt.Errorf("persist linear credential: %w", err))
		}
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

	// A failure after the clear puts the credential back: the request reports
	// the disconnect failed, so the org must still be connected.
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
		key, err := tx.Secrets.Get(ctx, orgID, integrations.KeyLinearAPIKey)
		if err != nil {
			return fmt.Errorf("read linear api key: %w", err)
		}
		had := method != "" || key != ""
		// TODO(TFAC-1022): when method is app_install, revoke the install's
		// refresh token in Linear and mark its org_linear_installs row removed
		// before the clear below — this route is that app's disconnect too.
		if err := integrations.ClearLinear(ctx, tx.Secrets, orgID); err != nil {
			return fmt.Errorf("clear credential: %w", err)
		}
		orgSet, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("load org settings: %w", err)
		}
		prevWorkspace := orgSet.LinearWorkspaceURLKey
		if orgSet.LinearWorkspaceID != "" || prevWorkspace != "" {
			orgSet.LinearWorkspaceID = ""
			orgSet.LinearWorkspaceURLKey = ""
			if err := saveLinearWorkspace(ctx, tx, orgID, orgSet); err != nil {
				return err
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
		if errors.Is(err, db.ErrOrgSettingsVersion) {
			writeLinearSettingsRace(w)
			return
		}
		internalError(w, "linear-access", err)
		return
	}

	s.kickLinearChanged(r, orgID)
	writeJSON(w, http.StatusOK, disconnectedResponse("linear"))
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
		return nil
	}); err != nil {
		return linearAccessStatus{}, err
	}

	status := linearAccessStatus{Connected: integrations.LinearSystemConfigured(creds)}
	if !status.Connected {
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

// snapshotLinearSecrets reads the org's stored Linear keys and returns a func
// that writes them back as they were — deleting a key that was absent. Local
// mode only, where secret writes land in the keychain outside the SQLite
// transaction and so survive its rollback. Any key that cannot be read fails
// the snapshot, since the restore could not put that key back. The restore
// outlives the request's context: a client that disconnects mid-write is
// exactly when it has to run.
func (s *Server) snapshotLinearSecrets(ctx context.Context, orgID string) (func(), error) {
	keys := []string{integrations.KeyLinearAPIKey, integrations.KeyLinearAuthMethod, integrations.KeyLinearAppInstall, integrations.KeyLinearBoundAs}
	prior := make(map[string]string, len(keys))
	for _, k := range keys {
		v, err := s.secrets.Get(ctx, orgID, k)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", k, err)
		}
		prior[k] = v
	}
	ctx = context.WithoutCancel(ctx)
	return func() {
		for k, v := range prior {
			var err error
			if v == "" {
				_, err = s.secrets.Delete(ctx, orgID, k)
			} else {
				err = s.secrets.Put(ctx, orgID, k, v, "")
			}
			if err != nil {
				serverLog.Warn("restore linear secret failed", "org", orgID, "key", k, "error", err)
			}
		}
	}, nil
}

// guardLocalLinearWrite prepares a Linear credential write in local mode, where
// the keychain sits outside the SQLite transaction: it takes
// linearCredentialMu and snapshots the stored keys. It returns the snapshot's
// restore, for the caller to run when the transaction fails, and the unlock to
// defer. A snapshot that cannot be taken refuses the write: a key it could not
// read is one it could not put back. Multi mode keeps secrets inside the
// transaction, so it returns a nil restore and a no-op unlock.
func (s *Server) guardLocalLinearWrite(ctx context.Context, orgID string) (restore, unlock func(), err error) {
	if runmode.Current() != runmode.ModeLocal {
		return nil, func() {}, nil
	}
	s.linearCredentialMu.Lock()
	restore, err = s.snapshotLinearSecrets(ctx, orgID)
	if err != nil {
		s.linearCredentialMu.Unlock()
		return nil, nil, err
	}
	return restore, s.linearCredentialMu.Unlock, nil
}

// saveLinearWorkspace writes the org settings row carrying the Linear workspace
// columns, guarded by the version the caller read inside the same
// transaction. The row holds every other org setting too, and an unguarded
// write would put back whatever a concurrent settings save just changed.
func saveLinearWorkspace(ctx context.Context, tx db.TxStores, orgID string, orgSet domain.OrgSettings) error {
	if _, err := tx.Orgs.UpdateSettingsVersioned(ctx, orgID, orgSet, orgSet.Version); err != nil {
		return fmt.Errorf("save org settings: %w", err)
	}
	return nil
}

// writeLinearSettingsRace answers a bind or unbind whose settings write lost
// to a concurrent settings save. Nothing was written, so the call is safe to
// repeat as is.
func writeLinearSettingsRace(w http.ResponseWriter) {
	httpx.WriteErrors(w, http.StatusConflict, httpx.ErrorItem{Reason: httpx.ReasonVersionConflict, Message: "the organization's settings changed during this request and nothing was saved — try again"})
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
