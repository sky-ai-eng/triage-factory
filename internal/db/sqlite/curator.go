package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// curatorStore is the SQLite impl of db.CuratorStore over the shared
// conversations / messages / claims tables. SQLite is single-tenant: the
// orgID assertion at each entry catches a confused caller, the two pools
// collapse onto the one connection, and the `...System` methods are plain
// methods here (no RLS to bypass). Claim writes still live only on the
// System methods so both dialects present the same door.
type curatorStore struct{ q queryer }

func newCuratorStore(q queryer) db.CuratorStore { return &curatorStore{q: q} }

var _ db.CuratorStore = (*curatorStore)(nil)

// --- Send path ---

func (s *curatorStore) GetOrCreateConversation(ctx context.Context, orgID, projectID, creatorUserID string) (*domain.Conversation, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var conv *domain.Conversation
	err := inTx(ctx, s.q, func(q queryer) error {
		got, err := getLiveCuratorConversation(ctx, q, projectID, creatorUserID)
		if err != nil {
			return err
		}
		if got != nil {
			conv = got
			return nil
		}
		id := uuid.New().String()
		now := time.Now().UTC()
		// team_id snapshots from the project at mint (point-in-time, like the
		// delegation side) so curator spend attributes to the project's team;
		// the llm_spend view attributes the messages ledger through this
		// snapshot. status is explicit NULL — a curator conversation has no
		// work lifecycle; its turn state derives from messages + claims.
		if _, err := q.ExecContext(ctx, `
			INSERT INTO conversations (
				id, org_id, type, creator_user_id, team_id, visibility,
				trigger_type, origin, runtime, status, project_id, started_at)
			VALUES (?, ?, 'curator', ?, (SELECT team_id FROM projects WHERE id = ?),
			        'private', 'manual', 'curator', 'sdk', NULL, ?, ?)
		`, id, orgID, creatorUserID, projectID, projectID, now); err != nil {
			return err
		}
		got, err = getLiveCuratorConversation(ctx, q, projectID, creatorUserID)
		if err != nil {
			return err
		}
		conv = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return conv, nil
}

func (s *curatorStore) GetLiveConversation(ctx context.Context, orgID, projectID, creatorUserID string) (*domain.Conversation, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return getLiveCuratorConversation(ctx, s.q, projectID, creatorUserID)
}

// getLiveCuratorConversation picks the OLDEST live conversation
// deterministically so two racing minters converge on the same row.
func getLiveCuratorConversation(ctx context.Context, q queryer, projectID, creatorUserID string) (*domain.Conversation, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, type, visibility, project_id, creator_user_id,
		       COALESCE(team_id, ''), COALESCE(sdk_session_id, ''), started_at
		FROM conversations
		WHERE project_id = ? AND creator_user_id = ? AND type = 'curator'
		  AND archived_at IS NULL
		ORDER BY started_at ASC, id ASC
		LIMIT 1
	`, projectID, creatorUserID)
	var c domain.Conversation
	err := row.Scan(&c.ID, &c.Type, &c.Visibility, &c.ProjectID, &c.CreatorUserID,
		&c.TeamID, &c.SessionID, &c.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *curatorStore) EnqueueUserMessage(ctx context.Context, orgID, conversationID, userID, content string) (int64, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, user_id, role, content, subtype, delivered, created_at)
		VALUES (?, ?, ?, 'user', ?, '', 0, ?)
	`, orgID, conversationID, userID, content, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// --- History ---

func (s *curatorStore) ListConversationMessages(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Withdrawn-pending rows (undelivered + inactive) never happened and are
	// hidden; delivered + inactive stays visible — that is retired history
	// (compaction, a dead-lettered turn's input) the transcript still shows.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteMessageColumns+`
		FROM messages
		WHERE conversation_id = ?
		  AND NOT (delivered = 0 AND window_state = 'inactive')
		ORDER BY COALESCE(seq, id) ASC
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

func (s *curatorStore) ListClaims(ctx context.Context, orgID, conversationID string) ([]domain.Claim, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteClaimColumns+`
		FROM claims
		WHERE conversation_id = ?
		ORDER BY claimed_at ASC, id ASC
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanClaimRows(rows)
}

// --- In-flight lookup + cancel ---

func (s *curatorStore) InFlightTurn(ctx context.Context, orgID, projectID, creatorUserID string) (*db.CuratorInFlightTurn, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	conv, err := getLiveCuratorConversation(ctx, s.q, projectID, creatorUserID)
	if err != nil || conv == nil {
		return nil, err
	}
	// Running wins over queued: the active claim's turn is the one a cancel
	// must reach (a queued turn behind it is targeted once it becomes the
	// front of the line).
	var active int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM claims WHERE conversation_id = ? AND released_at IS NULL
	`, conv.ID).Scan(&active); err != nil {
		return nil, err
	}
	if active > 0 {
		var msgID int64
		err := s.q.QueryRowContext(ctx, `
			SELECT id FROM messages
			WHERE conversation_id = ? AND role = 'user' AND subtype = '' AND delivered = 1
			ORDER BY id DESC LIMIT 1
		`, conv.ID).Scan(&msgID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &db.CuratorInFlightTurn{ConversationID: conv.ID, MessageID: msgID, Running: true}, nil
	}
	var msgID int64
	err = s.q.QueryRowContext(ctx, `
		SELECT id FROM messages
		WHERE conversation_id = ? AND role = 'user' AND subtype = '' AND delivered = 0
		ORDER BY id ASC LIMIT 1
	`, conv.ID).Scan(&msgID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &db.CuratorInFlightTurn{ConversationID: conv.ID, MessageID: msgID, Running: false}, nil
}

func (s *curatorStore) DeleteQueuedTurn(ctx context.Context, orgID, conversationID string, messageID int64) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM messages
		WHERE conversation_id = ? AND id = ? AND role = 'user' AND subtype = '' AND delivered = 0
	`, conversationID, messageID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *curatorStore) ArchiveLiveConversation(ctx context.Context, orgID, projectID, creatorUserID string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var archivedID string
	err := inTx(ctx, s.q, func(q queryer) error {
		conv, err := getLiveCuratorConversation(ctx, q, projectID, creatorUserID)
		if err != nil {
			return err
		}
		if conv == nil {
			return nil
		}
		var active int
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM claims WHERE conversation_id = ? AND released_at IS NULL
		`, conv.ID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return db.ErrCuratorInFlight
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM messages WHERE conversation_id = ? AND delivered = 0
		`, conv.ID); err != nil {
			return fmt.Errorf("delete undelivered rows: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE conversations SET archived_at = ? WHERE id = ?
		`, time.Now().UTC(), conv.ID); err != nil {
			return fmt.Errorf("archive conversation: %w", err)
		}
		archivedID = conv.ID
		return nil
	})
	if err != nil {
		return "", err
	}
	return archivedID, nil
}

// --- Dispatch ---

func (s *curatorStore) BeginTurn(ctx context.Context, orgID, projectID, conversationID string, messageID int64) (*db.CuratorTurnStart, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	start := &db.CuratorTurnStart{}
	err := inTx(ctx, s.q, func(q queryer) error {
		// FIRST statement: the delivery UPDATE. Forces RESERVED lock
		// acquisition before any read, closing the consume-vs-PATCH race the
		// same way the former pending-context consume did. The delivering
		// claim stamps the user row (single-active makes the subquery at
		// most one id); the stamp rides this tx, so a failed BeginTurn
		// leaves no stamp and a message-less failed claim stays
		// recognizable as a failed pickup.
		res, err := q.ExecContext(ctx, `
			UPDATE messages SET delivered = 1,
			       claim_id = (SELECT id FROM claims
			                   WHERE conversation_id = ? AND released_at IS NULL)
			WHERE conversation_id = ? AND id = ? AND role = 'user' AND subtype = '' AND delivered = 0
		`, conversationID, conversationID, messageID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return sql.ErrNoRows
		}
		if err := q.QueryRowContext(ctx, `
			SELECT content FROM messages WHERE id = ?
		`, messageID).Scan(&start.UserInput); err != nil {
			return err
		}

		rows, err := q.QueryContext(ctx, `
			SELECT id, COALESCE(metadata, '') FROM messages
			WHERE conversation_id = ? AND subtype = 'injection:context' AND delivered = 0
			ORDER BY id ASC
		`, conversationID)
		if err != nil {
			return err
		}
		consumed, err := scanContextChangeRows(rows)
		if err != nil {
			return err
		}
		for _, c := range consumed {
			if _, err := q.ExecContext(ctx, `
				UPDATE messages SET delivered = 1 WHERE id = ?
			`, c.MessageID); err != nil {
				return err
			}
		}
		start.Consumed = consumed

		if err := q.QueryRowContext(ctx, `
			SELECT COALESCE(sdk_session_id, '') FROM conversations WHERE id = ?
		`, conversationID).Scan(&start.SDKSessionID); err != nil {
			return err
		}

		p, err := scanCuratorProject(q.QueryRowContext(ctx, `
			SELECT id, name, description, pinned_repos, jira_project_key, linear_project_key, spec_authorship_blueprint_id, team_id, created_at, updated_at
			FROM projects WHERE id = ?
		`, projectID))
		if err != nil {
			return fmt.Errorf("read project: %w", err)
		}
		start.Project = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return start, nil
}

func (s *curatorStore) SetSDKSession(ctx context.Context, orgID, conversationID, sessionID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE conversations SET sdk_session_id = ? WHERE id = ?
	`, sessionID, conversationID)
	return err
}

func (s *curatorStore) FailedTurnAttemptsSystem(ctx context.Context, orgID, conversationID string, messageID int64) (int, string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, "", err
	}
	// Exact attribution via the mint-time message_id stamp — no time window,
	// so two queued turns on one conversation never count each other's
	// failures, and a withdrawn turn (message deleted → message_id NULLed)
	// matches nothing. The zero-message NOT EXISTS stays load-bearing: a
	// claim that delivered the turn also carries its message_id, and
	// counting a post-delivery run crash would cap the turn for a failure
	// that is not the input's fault.
	rows, err := s.q.QueryContext(ctx, `
		SELECT COALESCE(cl.error, '')
		FROM claims cl
		WHERE cl.conversation_id = ?
		  AND cl.released_at IS NOT NULL AND cl.outcome = 'failed'
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.claim_id = cl.id)
		  AND cl.message_id = ?
		ORDER BY cl.claimed_at DESC, cl.id DESC
	`, conversationID, messageID)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	count := 0
	lastError := ""
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return 0, "", err
		}
		if count == 0 {
			lastError = e
		}
		count++
	}
	return count, lastError, rows.Err()
}

func (s *curatorStore) DeadLetterTurnSystem(ctx context.Context, orgID, conversationID string, messageID int64, errMsg string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	matched := false
	err := inTx(ctx, s.q, func(q queryer) error {
		// Retire the message first, guarded on it still being queued: a
		// raced cancel that already deleted it (or a duplicate feed that
		// already delivered it) must leave the transcript untouched.
		// window_state='inactive' keeps the poisoned input out of assembly
		// and out of the claim predicate; delivered=1 keeps it visible
		// transcript history, stamped with the engagement that gave up.
		res, err := q.ExecContext(ctx, `
			UPDATE messages SET delivered = 1, window_state = 'inactive',
			    claim_id = (SELECT cl.id FROM claims cl
			                WHERE cl.conversation_id = ? AND cl.released_at IS NULL)
			WHERE conversation_id = ? AND id = ?
			  AND role = 'user' AND subtype = '' AND delivered = 0
		`, conversationID, conversationID, messageID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		matched = n > 0
		// The claim releases either way — see the Postgres twin.
		_, err = q.ExecContext(ctx, `
			UPDATE claims SET released_at = ?, outcome = 'failed', error = COALESCE(error, ?)
			WHERE conversation_id = ? AND released_at IS NULL
		`, time.Now().UTC(), nullIfEmpty(errMsg), conversationID)
		return err
	})
	return matched, err
}

func (s *curatorStore) ReleaseActiveTurnSystem(ctx context.Context, orgID, conversationID, outcome, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// The claim keeps only the engagement telemetry (duration/turns); the
	// turn's dollars settle as one lump on the claim's LAST message row —
	// the user row when nothing streamed (BeginTurn stamped it with this
	// claim). Every terminal write stamps, cancel included, so a turn
	// killed mid-stream still reports the spend it settled.
	//
	// Runtime-composed rows are skipped as targets: a turn that ended in an
	// API error or an interrupt ends on one, and settling there would bill
	// the turn to a model that never ran. There is always a non-synthetic
	// row to land on — the claim's user row carries no model at all.
	//
	// Of the rest, a row naming a real model wins over one naming none
	// however much newer the latter is (a turn cancelled mid-tool ends on a
	// NULL-model tool row); only the first keeps the lump in the per-model
	// breakdown. The nothing-streamed case still lands on the user row,
	// which is then the only candidate.
	released := false
	err := inTx(ctx, s.q, func(q queryer) error {
		var claimID string
		err := q.QueryRowContext(ctx, `
			SELECT id FROM claims WHERE conversation_id = ? AND released_at IS NULL
		`, conversationID).Scan(&claimID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE claims
			SET released_at = ?, outcome = ?, error = ?, duration_ms = ?, num_turns = ?
			WHERE id = ?
		`, time.Now().UTC(), outcome, nullIfEmpty(errMsg), durationMs, numTurns, claimID); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE messages SET cost_usd = ?
			WHERE id = (SELECT id FROM messages
			            WHERE claim_id = ? AND (model IS NULL OR model <> ?)
			            ORDER BY (model IS NOT NULL AND model <> ?) DESC, id DESC
			            LIMIT 1)
		`, costUSD, claimID, domain.ModelSynthetic, domain.ModelSynthetic); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

func (s *curatorStore) RevertTurnContext(ctx context.Context, orgID, conversationID string, consumed []domain.CuratorContextChange, auditMessageID int64) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		if len(consumed) > 0 {
			// Newer-wins merge: a change_type that gained a fresh undelivered
			// injection while this turn ran keeps the fresh row; resurrecting
			// the stale one would double the pending delta for that type.
			rows, err := q.QueryContext(ctx, `
				SELECT id, COALESCE(metadata, '') FROM messages
				WHERE conversation_id = ? AND subtype = 'injection:context' AND delivered = 0
			`, conversationID)
			if err != nil {
				return err
			}
			pending, err := scanContextChangeRows(rows)
			if err != nil {
				return err
			}
			pendingTypes := make(map[string]struct{}, len(pending))
			for _, p := range pending {
				pendingTypes[p.ChangeType] = struct{}{}
			}
			for _, c := range consumed {
				if _, superseded := pendingTypes[c.ChangeType]; superseded {
					continue
				}
				if _, err := q.ExecContext(ctx, `
					UPDATE messages SET delivered = 0
					WHERE conversation_id = ? AND id = ? AND subtype = 'injection:context'
				`, conversationID, c.MessageID); err != nil {
					return err
				}
			}
		}
		if auditMessageID > 0 {
			if _, err := q.ExecContext(ctx, `
				DELETE FROM messages
				WHERE conversation_id = ? AND id = ? AND subtype = 'context_change'
			`, conversationID, auditMessageID); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Boot sweeps ---
//
// Finding a claimable curator turn lives on the ONE claim loop
// (RunQueueStore.ClaimNextRun), not here.

// CancelStrandedTurnsForHomeSystem is inert in SQLite (local uses the global
// CancelOrphanedTurnsSystem boot sweep), but implemented for symmetry. An
// outcome/error release only: the ledger already holds whatever the turn
// streamed, and a stranded turn has no reported cost lump to settle.
func (s *curatorStore) CancelStrandedTurnsForHomeSystem(ctx context.Context, homeInstanceID string, bootEpoch int64, errMsg string) (int, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE claims
		SET released_at = ?, outcome = 'cancelled', error = COALESCE(error, ?)
		WHERE executor_id = ? AND boot_epoch < ? AND released_at IS NULL
		  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = claims.conversation_id AND c.type = 'curator')
	`, time.Now().UTC(), errMsg, homeInstanceID, bootEpoch)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *curatorStore) CancelOrphanedTurnsSystem(ctx context.Context) (int, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE claims
		SET released_at = ?, outcome = 'cancelled', error = COALESCE(error, 'process restarted')
		WHERE released_at IS NULL
		  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = claims.conversation_id AND c.type = 'curator')
	`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *curatorStore) DeleteQueuedTurnsForProjectSystem(ctx context.Context, orgID, projectID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	// Only plain queued turns go — pending injections aren't claimable work,
	// and delivered rows are transcript history the project cascade (or the
	// surviving archived project) keeps.
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM messages
		WHERE delivered = 0 AND role = 'user' AND subtype = ''
		  AND conversation_id IN (SELECT id FROM conversations
		                          WHERE org_id = ? AND project_id = ? AND type = 'curator')
	`, orgID, projectID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// --- Pending-context producer ---

func (s *curatorStore) QueueContextChangeSystem(ctx context.Context, orgID, projectID, changeType, baselineJSON string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	metadata, err := json.Marshal(map[string]string{
		"change_type":    changeType,
		"baseline_value": baselineJSON,
	})
	if err != nil {
		return fmt.Errorf("encode context-change metadata: %w", err)
	}
	return inTx(ctx, s.q, func(q queryer) error {
		convRows, err := q.QueryContext(ctx, `
			SELECT id, creator_user_id FROM conversations
			WHERE org_id = ? AND project_id = ? AND type = 'curator' AND archived_at IS NULL
			ORDER BY started_at ASC, id ASC
		`, orgID, projectID)
		if err != nil {
			return err
		}
		type liveConv struct{ id, creator string }
		var convs []liveConv
		if err := func() error {
			defer convRows.Close()
			for convRows.Next() {
				var c liveConv
				if err := convRows.Scan(&c.id, &c.creator); err != nil {
					return err
				}
				convs = append(convs, c)
			}
			return convRows.Err()
		}(); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, c := range convs {
			rows, err := q.QueryContext(ctx, `
				SELECT id, COALESCE(metadata, '') FROM messages
				WHERE conversation_id = ? AND subtype = 'injection:context' AND delivered = 0
			`, c.id)
			if err != nil {
				return err
			}
			pending, err := scanContextChangeRows(rows)
			if err != nil {
				return err
			}
			replaced := false
			for _, p := range pending {
				if p.ChangeType == changeType {
					if _, err := q.ExecContext(ctx, `
						UPDATE messages SET metadata = ?, created_at = ? WHERE id = ?
					`, string(metadata), now, p.MessageID); err != nil {
						return err
					}
					replaced = true
					break
				}
			}
			if replaced {
				continue
			}
			if _, err := q.ExecContext(ctx, `
				INSERT INTO messages (org_id, conversation_id, user_id, role, content, subtype, metadata, delivered, created_at)
				VALUES (?, ?, ?, 'user', '', 'injection:context', ?, 0, ?)
			`, orgID, c.id, c.creator, string(metadata), now); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Per-turn credentials ---

// PublishTurnCredPubKeySystem is inert in SQLite (N=1 role=all never stands a
// sidecar up), but implemented for interface + conformance symmetry. Guarded
// on "active claim with no key yet"; no doorbell (SQLite has no tf_ctl).
func (s *curatorStore) PublishTurnCredPubKeySystem(ctx context.Context, orgID, conversationID, pubkey string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE claims SET cred_pubkey = ?, phase = 'awaiting_credentials'
		WHERE org_id = ? AND conversation_id = ? AND released_at IS NULL
		  AND (cred_pubkey IS NULL OR cred_pubkey = '')
	`, pubkey, orgID, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *curatorStore) GetTurnProvisionInfoSystem(ctx context.Context, orgID, conversationID string) (*domain.CuratorTurnProvision, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, false, err
	}
	var p domain.CuratorTurnProvision
	var teamID, credPubKey sql.NullString
	err := s.q.QueryRowContext(ctx, `
		SELECT cl.conversation_id, c.org_id, COALESCE(c.project_id, ''), c.team_id, cl.executor_id, cl.cred_pubkey
		FROM claims cl
		JOIN conversations c ON c.id = cl.conversation_id
		WHERE c.org_id = ? AND cl.conversation_id = ? AND cl.released_at IS NULL
		  AND c.type = 'curator'
	`, orgID, conversationID).Scan(&p.ConversationID, &p.OrgID, &p.ProjectID, &teamID, &p.HomeInstanceID, &credPubKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	p.TeamID = teamID.String
	p.CredPubKey = credPubKey.String
	return &p, true, nil
}


// --- Project bundle import ---

func (s *curatorStore) ImportConversationStateSystem(ctx context.Context, orgID string, conv domain.Conversation, claims []domain.Claim, msgs []domain.Message) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		startedAt := conv.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now().UTC()
		}
		// team_id snapshots from the destination project row (the source
		// snapshot is meaningless in the importing install).
		if _, err := q.ExecContext(ctx, `
			INSERT INTO conversations (
				id, org_id, type, creator_user_id, team_id, visibility,
				trigger_type, origin, runtime, status, project_id, sdk_session_id, started_at)
			VALUES (?, ?, 'curator', ?, (SELECT team_id FROM projects WHERE id = ?),
			        'private', 'manual', 'curator', 'sdk', NULL, ?, ?, ?)
		`, conv.ID, orgID, conv.CreatorUserID, conv.ProjectID, conv.ProjectID,
			sqliteNullStr(conv.SessionID), startedAt); err != nil {
			return fmt.Errorf("import curator conversation %s: %w", conv.ID, err)
		}
		for _, cl := range claims {
			var released any
			if cl.ReleasedAt != nil {
				released = cl.ReleasedAt.UTC()
			}
			if _, err := q.ExecContext(ctx, `
				INSERT INTO claims (id, org_id, conversation_id, executor_id, boot_epoch,
				                    claimed_at, released_at, outcome, error,
				                    duration_ms, num_turns)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, cl.ID, orgID, conv.ID, cl.ExecutorID, cl.BootEpoch,
				cl.ClaimedAt.UTC(), released, nullIfEmpty(cl.Outcome), nullIfEmpty(cl.Error),
				sqliteNullInt(cl.DurationMs), sqliteNullInt(cl.NumTurns)); err != nil {
				return fmt.Errorf("import curator claim %s: %w", cl.ID, err)
			}
		}
		for i := range msgs {
			if err := importCuratorMessage(ctx, q, orgID, &msgs[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// importCuratorMessage mirrors agentRunStore.InsertMessage's column
// handling for a bundle row (delivered/window_state/seq preserved rather
// than defaulted — a compacted or seq-overridden message must not reappear
// active in assembly after a round-trip).
func importCuratorMessage(ctx context.Context, q queryer, orgID string, msg *domain.Message) error {
	var toolCallsJSON, metadataJSON, reasoningJSON, contentBlocksJSON sql.NullString
	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Metadata) > 0 {
		b, err := json.Marshal(msg.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.Reasoning) > 0 {
		b, err := json.Marshal(msg.Reasoning)
		if err != nil {
			return fmt.Errorf("marshal reasoning: %w", err)
		}
		reasoningJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(msg.ContentBlocks) > 0 {
		b, err := json.Marshal(msg.ContentBlocks)
		if err != nil {
			return fmt.Errorf("marshal content_blocks: %w", err)
		}
		contentBlocksJSON = sql.NullString{String: string(b), Valid: true}
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}
	delivered := true
	if msg.Delivered != nil {
		delivered = *msg.Delivered
	}
	windowState := msg.WindowState
	if windowState == "" {
		windowState = domain.MessageWindowActive
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, user_id, claim_id, role, content, subtype,
		                      tool_calls, tool_call_id, is_error, metadata, model,
		                      input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		                      cost_usd, created_at, reasoning, content_blocks, delivered,
		                      window_state, seq, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, orgID, msg.ConversationID, sqliteNullStr(msg.UserID), sqliteNullStr(msg.ClaimID),
		msg.Role, msg.Content, msg.Subtype,
		toolCallsJSON, sqliteNullStr(msg.ToolCallID), msg.IsError, metadataJSON,
		sqliteNullStr(msg.Model), sqliteNullInt(msg.InputTokens), sqliteNullInt(msg.OutputTokens),
		sqliteNullInt(msg.CacheReadTokens), sqliteNullInt(msg.CacheCreationTokens),
		sqliteNullFloat(msg.CostUSD), msg.CreatedAt, reasoningJSON, contentBlocksJSON, delivered,
		string(windowState), sqliteNullFloat(msg.Seq), sqliteNullInt(msg.DurationMs))
	if err != nil {
		return fmt.Errorf("import curator message: %w", err)
	}
	return nil
}

// --- Shared scan helpers ---

const sqliteClaimColumns = `id, org_id, conversation_id, executor_id, boot_epoch, phase, claimed_at,
	released_at, outcome, error, duration_ms, num_turns, created_at`

func scanClaimRows(rows *sql.Rows) ([]domain.Claim, error) {
	var out []domain.Claim
	for rows.Next() {
		var (
			c          domain.Claim
			phase      sql.NullString
			releasedAt sql.NullTime
			outcome    sql.NullString
			errMsg     sql.NullString
			durationMs sql.NullInt64
			numTurns   sql.NullInt64
		)
		if err := rows.Scan(
			&c.ID, &c.OrgID, &c.ConversationID, &c.ExecutorID, &c.BootEpoch, &phase, &c.ClaimedAt,
			&releasedAt, &outcome, &errMsg, &durationMs, &numTurns, &c.CreatedAt,
		); err != nil {
			return nil, err
		}
		if releasedAt.Valid {
			t := releasedAt.Time
			c.ReleasedAt = &t
		}
		c.Phase = phase.String
		c.Outcome = outcome.String
		c.Error = errMsg.String
		if durationMs.Valid {
			v := int(durationMs.Int64)
			c.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			c.NumTurns = &v
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanContextChangeRows drains (id, metadata) rows into the context-change
// projection, closing rows. A row whose metadata doesn't parse is skipped
// rather than failing the turn — a malformed injection can't hold the whole
// conversation hostage, and the renderer would drop it anyway.
func scanContextChangeRows(rows *sql.Rows) ([]domain.CuratorContextChange, error) {
	defer rows.Close()
	var out []domain.CuratorContextChange
	for rows.Next() {
		var (
			id   int64
			meta string
		)
		if err := rows.Scan(&id, &meta); err != nil {
			return nil, err
		}
		var payload struct {
			ChangeType    string `json:"change_type"`
			BaselineValue string `json:"baseline_value"`
		}
		if meta == "" || json.Unmarshal([]byte(meta), &payload) != nil || payload.ChangeType == "" {
			continue
		}
		out = append(out, domain.CuratorContextChange{
			MessageID:     id,
			ChangeType:    payload.ChangeType,
			BaselineValue: payload.BaselineValue,
		})
	}
	return out, rows.Err()
}

// scanCuratorProject reads a project row used by BeginTurn. Duplicates the
// project-store scan inline so the curator impl stays decoupled from
// projects.go's column lifetime.
func scanCuratorProject(row interface {
	Scan(dest ...any) error
}) (*domain.Project, error) {
	var (
		p               domain.Project
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID sql.NullString
		teamID          sql.NullString
		pinnedJSON      string
		createdAt       time.Time
		updatedAt       time.Time
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &pinnedJSON,
		&jiraKey, &linearKey, &specBlueprintID, &teamID,
		&createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.TeamID = teamID.String
	p.JiraProjectKey = jiraKey.String
	p.LinearProjectKey = linearKey.String
	p.SpecAuthorshipBlueprintID = specBlueprintID.String
	p.CreatedAt = createdAt
	p.UpdatedAt = updatedAt
	if pinnedJSON == "" {
		p.PinnedRepos = []string{}
	} else if err := json.Unmarshal([]byte(pinnedJSON), &p.PinnedRepos); err != nil {
		return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}
