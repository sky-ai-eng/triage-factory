package server

import (
	"context"
	"fmt"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/integrations"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// guardLocalSecretWrite prepares a credential write in local mode, where
// secret writes land in the keychain outside the SQLite transaction and so
// survive its rollback. It takes mu and snapshots keys, and returns the
// snapshot's restore, for the caller to run when the transaction fails, and
// the unlock to defer. The caller holds the lock across the transaction and
// the restore, so no other writer of those keys lands in between and is
// overwritten by the restore.
//
// mu is the integration's own mutex, taken by every writer of keys. A caller
// that already holds a lock every writer of keys takes passes nil.
//
// A snapshot that cannot be taken refuses the write: a key it could not read
// is one it could not put back. Multi mode keeps secrets inside the
// transaction, so it returns a nil restore and a no-op unlock.
func (s *Server) guardLocalSecretWrite(ctx context.Context, mu *sync.Mutex, orgID string, keys ...string) (restore, unlock func(), err error) {
	if runmode.Current() != runmode.ModeLocal {
		return nil, func() {}, nil
	}
	unlock = func() {}
	if mu != nil {
		mu.Lock()
		unlock = mu.Unlock
	}
	restore, err = s.snapshotSecrets(ctx, orgID, keys)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return restore, unlock, nil
}

// guardLocalLinearWrite is guardLocalSecretWrite over the Linear credential's
// keys, for the Linear bind and unbind.
func (s *Server) guardLocalLinearWrite(ctx context.Context, orgID string) (restore, unlock func(), err error) {
	return s.guardLocalSecretWrite(ctx, &s.linearCredentialMu, orgID, integrations.LinearKeys()...)
}

// guardLocalJiraWrite is guardLocalSecretWrite over the Jira credential's
// keys, for the Jira bind and unbind.
func (s *Server) guardLocalJiraWrite(ctx context.Context, orgID string) (restore, unlock func(), err error) {
	return s.guardLocalSecretWrite(ctx, &s.jiraCredentialMu, orgID, integrations.JiraKeys()...)
}

// guardLocalAnthropicWrite is guardLocalSecretWrite over the org's Anthropic
// API key, for the Anthropic bind and unbind.
func (s *Server) guardLocalAnthropicWrite(ctx context.Context, orgID string) (restore, unlock func(), err error) {
	return s.guardLocalSecretWrite(ctx, &s.anthropicCredentialMu, orgID, secretKeyAnthropicAPIKey)
}

// guardLocalBedrockWrite is guardLocalSecretWrite over every Bedrock key, for
// the three Bedrock binds and the unbind.
func (s *Server) guardLocalBedrockWrite(ctx context.Context, orgID string) (restore, unlock func(), err error) {
	return s.guardLocalSecretWrite(ctx, &s.bedrockCredentialMu, orgID, integrations.BedrockKeys()...)
}

// snapshotSecrets reads the org's stored keys and returns a func that writes
// them back as they were — deleting a key that was absent. Any key that cannot
// be read fails the snapshot. The restore outlives the request's context: a
// client that disconnects mid-write is exactly when it has to run. It writes
// the keys in the order given and carries on past a key it cannot write, so
// one keychain failure costs that key alone.
func (s *Server) snapshotSecrets(ctx context.Context, orgID string, keys []string) (func(), error) {
	prior := make([]string, len(keys))
	for i, k := range keys {
		v, err := s.secrets.Get(ctx, orgID, k)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", k, err)
		}
		prior[i] = v
	}
	ctx = context.WithoutCancel(ctx)
	return func() {
		for i, k := range keys {
			var err error
			if prior[i] == "" {
				_, err = s.secrets.Delete(ctx, orgID, k)
			} else {
				err = s.secrets.Put(ctx, orgID, k, prior[i], "")
			}
			if err != nil {
				serverLog.Error("restore secret failed; the stored credential may not match what the org had", "org", orgID, "key", k, "error", err)
			}
		}
	}, nil
}
