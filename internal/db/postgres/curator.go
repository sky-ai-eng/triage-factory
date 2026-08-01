package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/ctlbus"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// curatorStore is the Postgres impl of db.CuratorStore over the shared
// conversations / messages / claims tables.
//
// Pool split: q is the app pool — every send/history/cancel/dispatch-side
// message write runs claims-bound (WithTx / SyntheticClaimsWithTx), so the
// conversations private-visibility RLS arm scopes rows to the requesting
// creator, and tf_app's grant set (SELECT-only on claims) is the structural
// enforcement that claim writes can't leak onto this pool. admin is the
// RLS-bypassing pool for the `...System` methods: claim mint/release/stamp,
// the executor claim loop's cross-user scan, the boot sweeps, the
// pending-context producer (which writes into every creator's conversation),
// and the brain's provisioning reads. Both args collapse to the same queryer
// for the tx-bound composition.
type curatorStore struct {
	q     queryer
	admin queryer
}

func newCuratorStore(q, admin queryer) db.CuratorStore {
	return &curatorStore{q: q, admin: admin}
}

var _ db.CuratorStore = (*curatorStore)(nil)

// --- Send path ---

func (s *curatorStore) GetOrCreateConversation(ctx context.Context, orgID, projectID, creatorUserID string) (*domain.Conversation, error) {
	var conv *domain.Conversation
	err := inTx(ctx, s.q, func(q queryer) error {
		got, err := getLiveCuratorConversation(ctx, q, orgID, projectID, creatorUserID)
		if err != nil {
			return err
		}
		if got != nil {
			conv = got
			return nil
		}
		id := uuid.New().String()
		// team_id snapshots from the project at mint (point-in-time, like the
		// delegation side) so curator spend attributes to the project's team;
		// the llm_spend view attributes the messages ledger through this
		// snapshot. status is explicit NULL — a curator conversation has no
		// work lifecycle; its turn state derives from messages + claims. The
		// RLS INSERT policy pins creator_user_id to the caller's claims.
		if _, err := q.ExecContext(ctx, `
			INSERT INTO conversations (
				id, org_id, type, creator_user_id, team_id, visibility,
				trigger_type, origin, runtime, status, project_id, started_at)
			VALUES ($1, $2, 'curator', $3, (SELECT team_id FROM projects WHERE id = $4::uuid),
			        'private', 'manual', 'curator', 'sdk', NULL, $4, now())
		`, id, orgID, creatorUserID, projectID); err != nil {
			return err
		}
		got, err = getLiveCuratorConversation(ctx, q, orgID, projectID, creatorUserID)
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
	return getLiveCuratorConversation(ctx, s.q, orgID, projectID, creatorUserID)
}

// getLiveCuratorConversation picks the OLDEST live conversation
// deterministically so two racing minters converge on the same row.
func getLiveCuratorConversation(ctx context.Context, q queryer, orgID, projectID, creatorUserID string) (*domain.Conversation, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, type, visibility, COALESCE(project_id::text, ''), COALESCE(creator_user_id::text, ''),
		       COALESCE(team_id::text, ''), COALESCE(sdk_session_id, ''), started_at
		FROM conversations
		WHERE org_id = $1 AND project_id = $2 AND creator_user_id = $3
		  AND type = 'curator' AND archived_at IS NULL
		ORDER BY started_at ASC, id ASC
		LIMIT 1
	`, orgID, projectID, creatorUserID)
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
	var id int64
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO messages (org_id, conversation_id, user_id, role, content, subtype, delivered, created_at)
		VALUES ($1, $2, $3, 'user', $4, '', false, now())
		RETURNING id
	`, orgID, conversationID, userID, content).Scan(&id)
	return id, err
}

// --- History ---

func (s *curatorStore) ListConversationMessages(ctx context.Context, orgID, conversationID string) ([]domain.Message, error) {
	// Withdrawn-pending rows (undelivered + inactive) never happened and are
	// hidden; delivered + inactive stays visible — that is retired history
	// (compaction, a dead-lettered turn's input) the transcript still shows.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgMessageColumns+`
		FROM messages
		WHERE org_id = $1 AND conversation_id = $2
		  AND NOT (delivered = false AND window_state = 'inactive')
		ORDER BY COALESCE(seq, id) ASC
	`, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageRows(rows)
}

func (s *curatorStore) ListClaims(ctx context.Context, orgID, conversationID string) ([]domain.Claim, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgClaimColumns+`
		FROM claims
		WHERE org_id = $1 AND conversation_id = $2
		ORDER BY claimed_at ASC, id ASC
	`, orgID, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPgClaimRows(rows)
}

// --- In-flight lookup + cancel ---

func (s *curatorStore) InFlightTurn(ctx context.Context, orgID, projectID, creatorUserID string) (*db.CuratorInFlightTurn, error) {
	conv, err := getLiveCuratorConversation(ctx, s.q, orgID, projectID, creatorUserID)
	if err != nil || conv == nil {
		return nil, err
	}
	// Running wins over queued: the active claim's turn is the one a cancel
	// must reach (a queued turn behind it becomes the target once it is the
	// front of the line).
	var active int
	if err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL
	`, conv.ID).Scan(&active); err != nil {
		return nil, err
	}
	if active > 0 {
		var msgID int64
		err := s.q.QueryRowContext(ctx, `
			SELECT id FROM messages
			WHERE conversation_id = $1 AND role = 'user' AND subtype = '' AND delivered = true
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
		WHERE conversation_id = $1 AND role = 'user' AND subtype = '' AND delivered = false
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
	res, err := s.q.ExecContext(ctx, `
		DELETE FROM messages
		WHERE org_id = $1 AND conversation_id = $2 AND id = $3
		  AND role = 'user' AND subtype = '' AND delivered = false
	`, orgID, conversationID, messageID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *curatorStore) ArchiveLiveConversation(ctx context.Context, orgID, projectID, creatorUserID string) (string, error) {
	var archivedID string
	err := inTx(ctx, s.q, func(q queryer) error {
		conv, err := getLiveCuratorConversation(ctx, q, orgID, projectID, creatorUserID)
		if err != nil {
			return err
		}
		if conv == nil {
			return nil
		}
		var active int
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM claims WHERE conversation_id = $1 AND released_at IS NULL
		`, conv.ID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return db.ErrCuratorInFlight
		}
		if _, err := q.ExecContext(ctx, `
			DELETE FROM messages WHERE conversation_id = $1 AND delivered = false
		`, conv.ID); err != nil {
			return fmt.Errorf("delete undelivered rows: %w", err)
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE conversations SET archived_at = now() WHERE id = $1
		`, conv.ID); err != nil {
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

func (s *curatorStore) ClaimTurnSystem(ctx context.Context, orgID, conversationID string, messageID int64, executorID string, bootEpoch int64) (string, bool, error) {
	id := uuid.New().String()
	// The mint is guarded on the queued row still being undelivered: a raced
	// cancel deletes it (minting then would trip the message FK), and a
	// duplicate feed finds it delivered — both are skips, not engagements.
	res, err := s.admin.ExecContext(ctx, `
		INSERT INTO claims (id, org_id, conversation_id, message_id, executor_id, boot_epoch, claimed_at)
		SELECT $1, $2, $3, m.id, $5, $6, now()
		FROM messages m
		WHERE m.org_id = $2 AND m.conversation_id = $3 AND m.id = $4
		  AND m.role = 'user' AND m.subtype = '' AND m.delivered = false
		ON CONFLICT (conversation_id) WHERE released_at IS NULL DO NOTHING
	`, id, orgID, conversationID, messageID, executorID, bootEpoch)
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if n == 0 {
		return "", false, nil
	}
	return id, true, nil
}

func (s *curatorStore) BeginTurn(ctx context.Context, orgID, projectID, conversationID string, messageID int64) (*db.CuratorTurnStart, error) {
	start := &db.CuratorTurnStart{}
	err := inTx(ctx, s.q, func(q queryer) error {
		// The delivering claim stamps the user row (single-active makes the
		// subquery at most one id). The stamp rides this tx: a failed
		// BeginTurn leaves no stamp, so a message-less failed claim is
		// recognizable as a failed pickup, and a delivered turn's terminal
		// state stays derivable from the message even when nothing streams.
		var userInput string
		err := q.QueryRowContext(ctx, `
			UPDATE messages SET delivered = true,
			       claim_id = (SELECT id FROM claims
			                   WHERE conversation_id = $2 AND released_at IS NULL)
			WHERE org_id = $1 AND conversation_id = $2 AND id = $3
			  AND role = 'user' AND subtype = '' AND delivered = false
			RETURNING content
		`, orgID, conversationID, messageID).Scan(&userInput)
		if err != nil {
			return err
		}
		start.UserInput = userInput

		rows, err := q.QueryContext(ctx, `
			UPDATE messages SET delivered = true
			WHERE org_id = $1 AND conversation_id = $2
			  AND subtype = 'injection:context' AND delivered = false
			RETURNING id, COALESCE(metadata::text, '')
		`, orgID, conversationID)
		if err != nil {
			return err
		}
		consumed, err := scanPgContextChangeRows(rows)
		if err != nil {
			return err
		}
		start.Consumed = consumed

		if err := q.QueryRowContext(ctx, `
			SELECT COALESCE(sdk_session_id, '') FROM conversations WHERE id = $1
		`, conversationID).Scan(&start.SDKSessionID); err != nil {
			return err
		}

		p, err := scanPgCuratorProject(q.QueryRowContext(ctx, `
			SELECT id, name, description, pinned_repos::text,
			       jira_project_key, linear_project_key,
			       COALESCE(spec_authorship_blueprint_id::text, ''),
			       COALESCE(team_id::text, ''), created_at, updated_at
			FROM projects WHERE org_id = $1 AND id = $2
		`, orgID, projectID))
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
	_, err := s.q.ExecContext(ctx, `
		UPDATE conversations SET sdk_session_id = $1 WHERE org_id = $2 AND id = $3
	`, sessionID, orgID, conversationID)
	return err
}

func (s *curatorStore) FailedTurnAttemptsSystem(ctx context.Context, orgID, conversationID string, messageID int64) (int, string, error) {
	// Exact attribution via the mint-time message_id stamp — no time window,
	// so two queued turns on one conversation never count each other's
	// failures, and a withdrawn turn (message deleted → message_id NULLed)
	// matches nothing. The zero-message NOT EXISTS stays load-bearing: a
	// claim that delivered the turn also carries its message_id, and
	// counting a post-delivery run crash would cap the turn for a failure
	// that is not the input's fault.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT COALESCE(cl.error, '')
		FROM claims cl
		WHERE cl.org_id = $1 AND cl.conversation_id = $2
		  AND cl.released_at IS NOT NULL AND cl.outcome = 'failed'
		  AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.claim_id = cl.id)
		  AND cl.message_id = $3
		ORDER BY cl.claimed_at DESC, cl.id DESC
	`, orgID, conversationID, messageID)
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

func (s *curatorStore) DeadLetterTurnSystem(ctx context.Context, orgID, conversationID string, messageID int64, executorID string, bootEpoch int64, errMsg string) (bool, error) {
	matched := false
	err := inTx(ctx, s.admin, func(q queryer) error {
		// A live engagement owns the conversation — this gate mirrors the
		// dispatch's own claim-conflict skip, so the poisoned row waits
		// rather than fighting whatever is running.
		var active int
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM claims
			WHERE org_id = $1 AND conversation_id = $2 AND released_at IS NULL
		`, orgID, conversationID).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return nil
		}
		// Retire the message before minting anything: a raced cancel that
		// already deleted (or a duplicate feed that already delivered) the
		// row must leave zero writes behind. window_state='inactive' keeps
		// the poisoned input out of assembly and the claimable scan;
		// delivered=true keeps it visible transcript history.
		res, err := q.ExecContext(ctx, `
			UPDATE messages SET delivered = true, window_state = 'inactive'
			WHERE org_id = $1 AND conversation_id = $2 AND id = $3
			  AND role = 'user' AND subtype = '' AND delivered = false
		`, orgID, conversationID, messageID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		// The final claim is born released: it exists to carry the terminal
		// 'failed' the history synthesizer derives the turn's status from.
		claimID := uuid.New().String()
		if _, err := q.ExecContext(ctx, `
			INSERT INTO claims (id, org_id, conversation_id, message_id, executor_id, boot_epoch,
			                    claimed_at, released_at, outcome, error)
			VALUES ($1, $2, $3, $4, $5, $6, now(), now(), 'failed', $7)
		`, claimID, orgID, conversationID, messageID, executorID, bootEpoch, nullIfEmpty(errMsg)); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE messages SET claim_id = $1
			WHERE org_id = $2 AND conversation_id = $3 AND id = $4
		`, claimID, orgID, conversationID, messageID); err != nil {
			return err
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

func (s *curatorStore) ReleaseActiveTurnSystem(ctx context.Context, orgID, conversationID, outcome, errMsg string, costUSD float64, durationMs, numTurns int) (bool, error) {
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
	err := inTx(ctx, s.admin, func(q queryer) error {
		var claimID string
		err := q.QueryRowContext(ctx, `
			UPDATE claims
			SET released_at = now(), outcome = $1, error = $2, duration_ms = $3, num_turns = $4
			WHERE org_id = $5 AND conversation_id = $6 AND released_at IS NULL
			RETURNING id
		`, outcome, nullIfEmpty(errMsg), durationMs, numTurns, orgID, conversationID).Scan(&claimID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `
			UPDATE messages SET cost_usd = $1
			WHERE id = (SELECT m.id FROM messages m
			            WHERE m.claim_id = $2 AND m.model IS DISTINCT FROM $3
			            ORDER BY (m.model IS NOT NULL AND m.model <> $3) DESC, m.id DESC
			            LIMIT 1)
		`, costUSD, claimID, domain.ModelSynthetic); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}

func (s *curatorStore) RevertTurnContext(ctx context.Context, orgID, conversationID string, consumed []domain.CuratorContextChange, auditMessageID int64) error {
	return inTx(ctx, s.q, func(q queryer) error {
		if len(consumed) > 0 {
			// Newer-wins merge: a change_type that gained a fresh undelivered
			// injection while this turn ran keeps the fresh row; resurrecting
			// the stale one would double the pending delta for that type.
			rows, err := q.QueryContext(ctx, `
				SELECT id, COALESCE(metadata::text, '') FROM messages
				WHERE org_id = $1 AND conversation_id = $2
				  AND subtype = 'injection:context' AND delivered = false
			`, orgID, conversationID)
			if err != nil {
				return err
			}
			pending, err := scanPgContextChangeRows(rows)
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
					UPDATE messages SET delivered = false
					WHERE org_id = $1 AND conversation_id = $2 AND id = $3
					  AND subtype = 'injection:context'
				`, orgID, conversationID, c.MessageID); err != nil {
					return err
				}
			}
		}
		if auditMessageID > 0 {
			if _, err := q.ExecContext(ctx, `
				DELETE FROM messages
				WHERE org_id = $1 AND conversation_id = $2 AND id = $3
				  AND subtype = 'context_change'
			`, orgID, conversationID, auditMessageID); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Executor claim loop / sweeps ---

func (s *curatorStore) ListClaimableTurnsForHomeSystem(ctx context.Context, homeInstanceID string) ([]domain.CuratorTurn, error) {
	rows, err := s.admin.QueryContext(ctx, `
		SELECT m.id, m.conversation_id, c.org_id, COALESCE(c.project_id::text, ''), COALESCE(c.creator_user_id::text, '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN curator_homes h ON h.org_id = c.org_id::text AND h.project_id = c.project_id::text
		WHERE c.type = 'curator' AND c.archived_at IS NULL
		  AND m.role = 'user' AND m.subtype = '' AND m.delivered = false
		  AND h.home_instance_id = $1
		  AND NOT EXISTS (SELECT 1 FROM claims cl WHERE cl.conversation_id = c.id AND cl.released_at IS NULL)
		  AND m.id = (SELECT MIN(m2.id) FROM messages m2
		              WHERE m2.conversation_id = c.id AND m2.role = 'user'
		                AND m2.subtype = '' AND m2.delivered = false)
		ORDER BY m.id ASC
	`, homeInstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CuratorTurn
	for rows.Next() {
		var t domain.CuratorTurn
		if err := rows.Scan(&t.MessageID, &t.ConversationID, &t.OrgID, &t.ProjectID, &t.CreatorUserID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CancelStrandedTurnsForHomeSystem is an outcome/error release only: the
// ledger already holds whatever the stranded turn streamed, and a turn whose
// home died has no reported cost lump to settle.
func (s *curatorStore) CancelStrandedTurnsForHomeSystem(ctx context.Context, homeInstanceID string, bootEpoch int64, errMsg string) (int, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE claims
		SET released_at = now(), outcome = 'cancelled', error = COALESCE(error, $1)
		WHERE executor_id = $2 AND boot_epoch < $3 AND released_at IS NULL
		  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = claims.conversation_id AND c.type = 'curator')
	`, errMsg, homeInstanceID, bootEpoch)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *curatorStore) CancelOrphanedTurnsSystem(ctx context.Context) (int, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE claims
		SET released_at = now(), outcome = 'cancelled', error = COALESCE(error, 'process restarted')
		WHERE released_at IS NULL
		  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = claims.conversation_id AND c.type = 'curator')
	`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *curatorStore) DeleteQueuedTurnsForProjectSystem(ctx context.Context, orgID, projectID string) (int, error) {
	// Only plain queued turns go — pending injections aren't claimable work,
	// and delivered rows are transcript history the project cascade (or the
	// surviving archived project) keeps.
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM messages
		WHERE delivered = false AND role = 'user' AND subtype = ''
		  AND conversation_id IN (SELECT id FROM conversations
		                          WHERE org_id = $1 AND project_id = $2 AND type = 'curator')
	`, orgID, projectID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// --- Pending-context producer ---

func (s *curatorStore) QueueContextChangeSystem(ctx context.Context, orgID, projectID, changeType, baselineJSON string) error {
	metadata, err := json.Marshal(map[string]string{
		"change_type":    changeType,
		"baseline_value": baselineJSON,
	})
	if err != nil {
		return fmt.Errorf("encode context-change metadata: %w", err)
	}
	return inTx(ctx, s.admin, func(q queryer) error {
		convRows, err := q.QueryContext(ctx, `
			SELECT id, COALESCE(creator_user_id::text, '') FROM conversations
			WHERE org_id = $1 AND project_id = $2 AND type = 'curator' AND archived_at IS NULL
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
		for _, c := range convs {
			rows, err := q.QueryContext(ctx, `
				SELECT id, COALESCE(metadata::text, '') FROM messages
				WHERE conversation_id = $1 AND subtype = 'injection:context' AND delivered = false
			`, c.id)
			if err != nil {
				return err
			}
			pending, err := scanPgContextChangeRows(rows)
			if err != nil {
				return err
			}
			replaced := false
			for _, p := range pending {
				if p.ChangeType == changeType {
					if _, err := q.ExecContext(ctx, `
						UPDATE messages SET metadata = $1, created_at = now() WHERE id = $2
					`, metadata, p.MessageID); err != nil {
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
				VALUES ($1, $2, $3, 'user', '', 'injection:context', $4, false, now())
			`, orgID, c.id, nullIfEmpty(c.creator), metadata); err != nil {
				return err
			}
		}
		return nil
	})
}

// --- Per-turn credentials ---

func (s *curatorStore) PublishTurnCredPubKeySystem(ctx context.Context, orgID, conversationID, pubkey string) (bool, error) {
	res, err := s.admin.ExecContext(ctx, `
		UPDATE claims SET cred_pubkey = $1, phase = 'awaiting_credentials'
		WHERE org_id = $2 AND conversation_id = $3 AND released_at IS NULL
		  AND (cred_pubkey IS NULL OR cred_pubkey = '')
	`, pubkey, orgID, conversationID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 && pubkey != "" {
		// Best-effort doorbell so the brain provisions this turn without
		// waiting for the backstop sweep; the payload carries the
		// conversation id — the claim-credentials channel's key.
		_ = ctlbus.Publish(ctx, s.admin, ctlbus.Message{Kind: "curator_cred_request", OrgID: orgID, RunID: conversationID})
	}
	return n > 0, nil
}

func (s *curatorStore) GetTurnProvisionInfoSystem(ctx context.Context, orgID, conversationID string) (*domain.CuratorTurnProvision, bool, error) {
	var p domain.CuratorTurnProvision
	err := s.admin.QueryRowContext(ctx, `
		SELECT cl.conversation_id, c.org_id, COALESCE(c.project_id::text, ''),
		       COALESCE(c.team_id::text, ''), cl.executor_id, COALESCE(cl.cred_pubkey, '')
		FROM claims cl
		JOIN conversations c ON c.id = cl.conversation_id
		WHERE c.org_id = $1 AND cl.conversation_id = $2 AND cl.released_at IS NULL
		  AND c.type = 'curator'
	`, orgID, conversationID).Scan(&p.ConversationID, &p.OrgID, &p.ProjectID, &p.TeamID, &p.HomeInstanceID, &p.CredPubKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &p, true, nil
}

// --- Project bundle import ---

func (s *curatorStore) ImportConversationStateSystem(ctx context.Context, orgID string, conv domain.Conversation, claims []domain.Claim, msgs []domain.Message) error {
	return inTx(ctx, s.admin, func(q queryer) error {
		startedAt := conv.StartedAt
		if startedAt.IsZero() {
			startedAt = time.Now().UTC()
		}
		// team_id snapshots from the destination project row — the source
		// install's snapshot is meaningless here.
		if _, err := q.ExecContext(ctx, `
			INSERT INTO conversations (
				id, org_id, type, creator_user_id, team_id, visibility,
				trigger_type, origin, runtime, status, project_id, sdk_session_id, started_at)
			VALUES ($1, $2, 'curator', $3, (SELECT team_id FROM projects WHERE id = $4::uuid),
			        'private', 'manual', 'curator', 'sdk', NULL, $4, $5, $6)
		`, conv.ID, orgID, conv.CreatorUserID, conv.ProjectID,
			nullIfEmpty(conv.SessionID), startedAt); err != nil {
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
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			`, cl.ID, orgID, conv.ID, cl.ExecutorID, cl.BootEpoch,
				cl.ClaimedAt.UTC(), released, nullIfEmpty(cl.Outcome), nullIfEmpty(cl.Error),
				nullIntPtr(cl.DurationMs), nullIntPtr(cl.NumTurns)); err != nil {
				return fmt.Errorf("import curator claim %s: %w", cl.ID, err)
			}
		}
		for i := range msgs {
			if _, err := insertRunMessage(ctx, q, orgID, &msgs[i]); err != nil {
				return fmt.Errorf("import curator message: %w", err)
			}
		}
		return nil
	})
}

// --- Shared scan helpers ---

const pgClaimColumns = `id, org_id, conversation_id, executor_id, boot_epoch, phase, claimed_at,
	released_at, outcome, error, duration_ms, num_turns, created_at`

func scanPgClaimRows(rows *sql.Rows) ([]domain.Claim, error) {
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

// scanPgContextChangeRows drains (id, metadata) rows into the context-change
// projection, closing rows. A row whose metadata doesn't parse is skipped
// rather than failing the turn — the renderer would drop it anyway.
func scanPgContextChangeRows(rows *sql.Rows) ([]domain.CuratorContextChange, error) {
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

// scanPgCuratorProject reads a project row used by BeginTurn. Duplicates the
// project-store scan inline so the curator impl stays decoupled from
// projects.go's column lifetime.
func scanPgCuratorProject(row interface {
	Scan(dest ...any) error
}) (*domain.Project, error) {
	var (
		p               domain.Project
		jiraKey         sql.NullString
		linearKey       sql.NullString
		specBlueprintID string
		teamID          string
		pinnedJSON      sql.NullString
	)
	err := row.Scan(
		&p.ID, &p.Name, &p.Description, &pinnedJSON,
		&jiraKey, &linearKey, &specBlueprintID, &teamID,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.TeamID = teamID
	p.JiraProjectKey = jiraKey.String
	p.LinearProjectKey = linearKey.String
	p.SpecAuthorshipBlueprintID = specBlueprintID
	if pinnedJSON.Valid && pinnedJSON.String != "" {
		if err := json.Unmarshal([]byte(pinnedJSON.String), &p.PinnedRepos); err != nil {
			return nil, fmt.Errorf("unmarshal pinned_repos: %w", err)
		}
	}
	if p.PinnedRepos == nil {
		p.PinnedRepos = []string{}
	}
	return &p, nil
}
