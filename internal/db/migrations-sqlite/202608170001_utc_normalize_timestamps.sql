-- +goose Up
-- Rewrite every stored timestamp to offset-free UTC text.
--
-- The DSN sets _time_format=sqlite, so the driver serializes a bound
-- time.Time as "2006-01-02 15:04:05.999999999-07:00" — with the WRITER's UTC
-- offset attached. Most of the store layer bound time.Now().UTC() and a
-- minority bound a bare time.Now(), so on any machine whose zone is not UTC
-- a single column could hold values written in two different zones, plus a
-- third shape from the schema's own CURRENT_TIMESTAMP defaults (naive UTC).
--
-- The write sites are fixed and a ratchet keeps them fixed. That leaves the
-- rows already on disk: a laptop that ran an older build holds local-offset
-- values beside the UTC ones written from now on. Reads that ORDER BY a raw
-- timestamp column — the task queue's closed_at, the events feed's
-- COALESCE(occurred_at, created_at), the factory belt — compare those two
-- shapes as text and interleave them wrongly, by exactly the offset. So the
-- rows have to be brought onto one shape rather than tolerated on read:
-- wrapping every ordered read in datetime() would defeat the indexes those
-- reads walk.
--
-- strftime('%Y-%m-%d %H:%M:%f', c) is the normalizer. It resolves each of the
-- shapes that can be on disk — offset-bearing driver text, naive
-- CURRENT_TIMESTAMP text, ISO-8601 with a T and a Z — to the same instant
-- rendered offset-free in UTC, and it is idempotent, so a second run is a
-- no-op and an already-UTC value is left on its own instant. Scans are
-- unaffected: the driver reads an offset-free DATETIME as UTC.
--
-- Three deliberate properties of the statements below:
--
--   * typeof(c) = 'text' — a numeric value would be read by strftime as a
--     Julian day, which is not what any writer here meant.
--   * strftime(...) IS NOT NULL — an unparseable value (the legacy Go
--     time.String() rendering that _time_format=sqlite was added to stop
--     producing) yields NULL, and overwriting a timestamp with NULL loses
--     it. Such a value cannot exist in a database this binary opens — the
--     v1.11.0 baseline postdates that format by months and the brick check
--     refuses anything older — but the guard costs nothing and the failure
--     it prevents is silent.
--   * UPDATE OR IGNORE — instance_stats and sandbox_stats carry a uniqueness
--     constraint that includes the timestamp. Truncating to milliseconds
--     could in principle collide two samples of the same source, which would
--     abort the migration; skipping that row instead leaves it on its old
--     value, which is the same posture as the NULL guard. Both tables sample
--     on multi-second intervals, so this is a backstop, not an expectation.
--
-- Millisecond truncation is accepted: it is SQLite's own precision, and every
-- ordered read in the codebase carries an id tiebreaker.
--
-- The column list is every DATETIME/TIMESTAMP column in the schema as of this
-- version, derived from pragma_table_info rather than written out by hand;
-- TestMigrate_UTCNormalizeCoversEveryTimestampColumn re-derives it and fails
-- if the two disagree. goose_db_version.tstamp is excluded — it is goose's
-- own bookkeeping, not ours.
UPDATE OR IGNORE access_change_log SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE agents SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE agents SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE artifacts SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE artifacts SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE auth_events SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE blueprint_runs SET started_at = strftime('%Y-%m-%d %H:%M:%f', started_at)
 WHERE typeof(started_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', started_at) IS NOT NULL
   AND started_at <> strftime('%Y-%m-%d %H:%M:%f', started_at);
UPDATE OR IGNORE blueprint_runs SET completed_at = strftime('%Y-%m-%d %H:%M:%f', completed_at)
 WHERE typeof(completed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', completed_at) IS NOT NULL
   AND completed_at <> strftime('%Y-%m-%d %H:%M:%f', completed_at);

UPDATE OR IGNORE blueprint_steps SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE blueprints SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE blueprints SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);
UPDATE OR IGNORE blueprints SET deleted_at = strftime('%Y-%m-%d %H:%M:%f', deleted_at)
 WHERE typeof(deleted_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', deleted_at) IS NOT NULL
   AND deleted_at <> strftime('%Y-%m-%d %H:%M:%f', deleted_at);

UPDATE OR IGNORE claims SET claimed_at = strftime('%Y-%m-%d %H:%M:%f', claimed_at)
 WHERE typeof(claimed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', claimed_at) IS NOT NULL
   AND claimed_at <> strftime('%Y-%m-%d %H:%M:%f', claimed_at);
UPDATE OR IGNORE claims SET released_at = strftime('%Y-%m-%d %H:%M:%f', released_at)
 WHERE typeof(released_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', released_at) IS NOT NULL
   AND released_at <> strftime('%Y-%m-%d %H:%M:%f', released_at);
UPDATE OR IGNORE claims SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE conversation_memory SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE conversation_memory_entities SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE conversation_permissions SET requested_at = strftime('%Y-%m-%d %H:%M:%f', requested_at)
 WHERE typeof(requested_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', requested_at) IS NOT NULL
   AND requested_at <> strftime('%Y-%m-%d %H:%M:%f', requested_at);
UPDATE OR IGNORE conversation_permissions SET expires_at = strftime('%Y-%m-%d %H:%M:%f', expires_at)
 WHERE typeof(expires_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', expires_at) IS NOT NULL
   AND expires_at <> strftime('%Y-%m-%d %H:%M:%f', expires_at);
UPDATE OR IGNORE conversation_permissions SET decided_at = strftime('%Y-%m-%d %H:%M:%f', decided_at)
 WHERE typeof(decided_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', decided_at) IS NOT NULL
   AND decided_at <> strftime('%Y-%m-%d %H:%M:%f', decided_at);

UPDATE OR IGNORE conversation_worktrees SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE conversations SET started_at = strftime('%Y-%m-%d %H:%M:%f', started_at)
 WHERE typeof(started_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', started_at) IS NOT NULL
   AND started_at <> strftime('%Y-%m-%d %H:%M:%f', started_at);
UPDATE OR IGNORE conversations SET completed_at = strftime('%Y-%m-%d %H:%M:%f', completed_at)
 WHERE typeof(completed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', completed_at) IS NOT NULL
   AND completed_at <> strftime('%Y-%m-%d %H:%M:%f', completed_at);
UPDATE OR IGNORE conversations SET parked_at = strftime('%Y-%m-%d %H:%M:%f', parked_at)
 WHERE typeof(parked_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', parked_at) IS NOT NULL
   AND parked_at <> strftime('%Y-%m-%d %H:%M:%f', parked_at);
UPDATE OR IGNORE conversations SET archived_at = strftime('%Y-%m-%d %H:%M:%f', archived_at)
 WHERE typeof(archived_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', archived_at) IS NOT NULL
   AND archived_at <> strftime('%Y-%m-%d %H:%M:%f', archived_at);
UPDATE OR IGNORE conversations SET queued_at = strftime('%Y-%m-%d %H:%M:%f', queued_at)
 WHERE typeof(queued_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', queued_at) IS NOT NULL
   AND queued_at <> strftime('%Y-%m-%d %H:%M:%f', queued_at);

UPDATE OR IGNORE curator_homes SET homed_at = strftime('%Y-%m-%d %H:%M:%f', homed_at)
 WHERE typeof(homed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', homed_at) IS NOT NULL
   AND homed_at <> strftime('%Y-%m-%d %H:%M:%f', homed_at);
UPDATE OR IGNORE curator_homes SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE entities SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE entities SET last_polled_at = strftime('%Y-%m-%d %H:%M:%f', last_polled_at)
 WHERE typeof(last_polled_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', last_polled_at) IS NOT NULL
   AND last_polled_at <> strftime('%Y-%m-%d %H:%M:%f', last_polled_at);
UPDATE OR IGNORE entities SET closed_at = strftime('%Y-%m-%d %H:%M:%f', closed_at)
 WHERE typeof(closed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', closed_at) IS NOT NULL
   AND closed_at <> strftime('%Y-%m-%d %H:%M:%f', closed_at);
UPDATE OR IGNORE entities SET classified_at = strftime('%Y-%m-%d %H:%M:%f', classified_at)
 WHERE typeof(classified_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', classified_at) IS NOT NULL
   AND classified_at <> strftime('%Y-%m-%d %H:%M:%f', classified_at);

UPDATE OR IGNORE entity_links SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE event_handlers SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE event_handlers SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);
UPDATE OR IGNORE event_handlers SET deleted_at = strftime('%Y-%m-%d %H:%M:%f', deleted_at)
 WHERE typeof(deleted_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', deleted_at) IS NOT NULL
   AND deleted_at <> strftime('%Y-%m-%d %H:%M:%f', deleted_at);

UPDATE OR IGNORE event_queue SET enqueued_at = strftime('%Y-%m-%d %H:%M:%f', enqueued_at)
 WHERE typeof(enqueued_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', enqueued_at) IS NOT NULL
   AND enqueued_at <> strftime('%Y-%m-%d %H:%M:%f', enqueued_at);
UPDATE OR IGNORE event_queue SET claimed_at = strftime('%Y-%m-%d %H:%M:%f', claimed_at)
 WHERE typeof(claimed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', claimed_at) IS NOT NULL
   AND claimed_at <> strftime('%Y-%m-%d %H:%M:%f', claimed_at);
UPDATE OR IGNORE event_queue SET processed_at = strftime('%Y-%m-%d %H:%M:%f', processed_at)
 WHERE typeof(processed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', processed_at) IS NOT NULL
   AND processed_at <> strftime('%Y-%m-%d %H:%M:%f', processed_at);

UPDATE OR IGNORE events SET occurred_at = strftime('%Y-%m-%d %H:%M:%f', occurred_at)
 WHERE typeof(occurred_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', occurred_at) IS NOT NULL
   AND occurred_at <> strftime('%Y-%m-%d %H:%M:%f', occurred_at);
UPDATE OR IGNORE events SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE external_actions SET occurred_at = strftime('%Y-%m-%d %H:%M:%f', occurred_at)
 WHERE typeof(occurred_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', occurred_at) IS NOT NULL
   AND occurred_at <> strftime('%Y-%m-%d %H:%M:%f', occurred_at);

UPDATE OR IGNORE github_webhook_deliveries SET received_at = strftime('%Y-%m-%d %H:%M:%f', received_at)
 WHERE typeof(received_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', received_at) IS NOT NULL
   AND received_at <> strftime('%Y-%m-%d %H:%M:%f', received_at);

UPDATE OR IGNORE instance_stats SET at = strftime('%Y-%m-%d %H:%M:%f', at)
 WHERE typeof(at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', at) IS NOT NULL
   AND at <> strftime('%Y-%m-%d %H:%M:%f', at);

UPDATE OR IGNORE instances SET started_at = strftime('%Y-%m-%d %H:%M:%f', started_at)
 WHERE typeof(started_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', started_at) IS NOT NULL
   AND started_at <> strftime('%Y-%m-%d %H:%M:%f', started_at);
UPDATE OR IGNORE instances SET last_heartbeat_at = strftime('%Y-%m-%d %H:%M:%f', last_heartbeat_at)
 WHERE typeof(last_heartbeat_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', last_heartbeat_at) IS NOT NULL
   AND last_heartbeat_at <> strftime('%Y-%m-%d %H:%M:%f', last_heartbeat_at);

UPDATE OR IGNORE jira_project_status_rules SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE memberships SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE messages SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE operators SET added_at = strftime('%Y-%m-%d %H:%M:%f', added_at)
 WHERE typeof(added_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', added_at) IS NOT NULL
   AND added_at <> strftime('%Y-%m-%d %H:%M:%f', added_at);

UPDATE OR IGNORE org_github_app_installations SET installed_at = strftime('%Y-%m-%d %H:%M:%f', installed_at)
 WHERE typeof(installed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', installed_at) IS NOT NULL
   AND installed_at <> strftime('%Y-%m-%d %H:%M:%f', installed_at);
UPDATE OR IGNORE org_github_app_installations SET removed_at = strftime('%Y-%m-%d %H:%M:%f', removed_at)
 WHERE typeof(removed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', removed_at) IS NOT NULL
   AND removed_at <> strftime('%Y-%m-%d %H:%M:%f', removed_at);
UPDATE OR IGNORE org_github_app_installations SET suspended_at = strftime('%Y-%m-%d %H:%M:%f', suspended_at)
 WHERE typeof(suspended_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', suspended_at) IS NOT NULL
   AND suspended_at <> strftime('%Y-%m-%d %H:%M:%f', suspended_at);

UPDATE OR IGNORE org_github_apps SET registered_at = strftime('%Y-%m-%d %H:%M:%f', registered_at)
 WHERE typeof(registered_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', registered_at) IS NOT NULL
   AND registered_at <> strftime('%Y-%m-%d %H:%M:%f', registered_at);

UPDATE OR IGNORE org_jira_apps SET registered_at = strftime('%Y-%m-%d %H:%M:%f', registered_at)
 WHERE typeof(registered_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', registered_at) IS NOT NULL
   AND registered_at <> strftime('%Y-%m-%d %H:%M:%f', registered_at);

UPDATE OR IGNORE org_memberships SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE org_settings SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE orgs SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE orgs SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE pending_firings SET queued_at = strftime('%Y-%m-%d %H:%M:%f', queued_at)
 WHERE typeof(queued_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', queued_at) IS NOT NULL
   AND queued_at <> strftime('%Y-%m-%d %H:%M:%f', queued_at);
UPDATE OR IGNORE pending_firings SET drained_at = strftime('%Y-%m-%d %H:%M:%f', drained_at)
 WHERE typeof(drained_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', drained_at) IS NOT NULL
   AND drained_at <> strftime('%Y-%m-%d %H:%M:%f', drained_at);
UPDATE OR IGNORE pending_firings SET claimed_at = strftime('%Y-%m-%d %H:%M:%f', claimed_at)
 WHERE typeof(claimed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', claimed_at) IS NOT NULL
   AND claimed_at <> strftime('%Y-%m-%d %H:%M:%f', claimed_at);

UPDATE OR IGNORE placement_overrides SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE poll_readiness SET restarted_at = strftime('%Y-%m-%d %H:%M:%f', restarted_at)
 WHERE typeof(restarted_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', restarted_at) IS NOT NULL
   AND restarted_at <> strftime('%Y-%m-%d %H:%M:%f', restarted_at);
UPDATE OR IGNORE poll_readiness SET last_poll_at = strftime('%Y-%m-%d %H:%M:%f', last_poll_at)
 WHERE typeof(last_poll_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', last_poll_at) IS NOT NULL
   AND last_poll_at <> strftime('%Y-%m-%d %H:%M:%f', last_poll_at);

UPDATE OR IGNORE poller_state SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE projects SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE projects SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE prompts SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE prompts SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);
UPDATE OR IGNORE prompts SET deleted_at = strftime('%Y-%m-%d %H:%M:%f', deleted_at)
 WHERE typeof(deleted_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', deleted_at) IS NOT NULL
   AND deleted_at <> strftime('%Y-%m-%d %H:%M:%f', deleted_at);

UPDATE OR IGNORE reachable_repositories SET observed_at = strftime('%Y-%m-%d %H:%M:%f', observed_at)
 WHERE typeof(observed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', observed_at) IS NOT NULL
   AND observed_at <> strftime('%Y-%m-%d %H:%M:%f', observed_at);

UPDATE OR IGNORE reachable_scopes SET refreshed_at = strftime('%Y-%m-%d %H:%M:%f', refreshed_at)
 WHERE typeof(refreshed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', refreshed_at) IS NOT NULL
   AND refreshed_at <> strftime('%Y-%m-%d %H:%M:%f', refreshed_at);

UPDATE OR IGNORE repositories SET profiled_at = strftime('%Y-%m-%d %H:%M:%f', profiled_at)
 WHERE typeof(profiled_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', profiled_at) IS NOT NULL
   AND profiled_at <> strftime('%Y-%m-%d %H:%M:%f', profiled_at);
UPDATE OR IGNORE repositories SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);
UPDATE OR IGNORE repositories SET pulls_polled_at = strftime('%Y-%m-%d %H:%M:%f', pulls_polled_at)
 WHERE typeof(pulls_polled_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', pulls_polled_at) IS NOT NULL
   AND pulls_polled_at <> strftime('%Y-%m-%d %H:%M:%f', pulls_polled_at);

UPDATE OR IGNORE sandbox_stats SET at = strftime('%Y-%m-%d %H:%M:%f', at)
 WHERE typeof(at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', at) IS NOT NULL
   AND at <> strftime('%Y-%m-%d %H:%M:%f', at);

UPDATE OR IGNORE seat_claims SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE swipe_events SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE system_llm_runs SET started_at = strftime('%Y-%m-%d %H:%M:%f', started_at)
 WHERE typeof(started_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', started_at) IS NOT NULL
   AND started_at <> strftime('%Y-%m-%d %H:%M:%f', started_at);
UPDATE OR IGNORE system_llm_runs SET completed_at = strftime('%Y-%m-%d %H:%M:%f', completed_at)
 WHERE typeof(completed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', completed_at) IS NOT NULL
   AND completed_at <> strftime('%Y-%m-%d %H:%M:%f', completed_at);

UPDATE OR IGNORE task_events SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE tasks SET snooze_until = strftime('%Y-%m-%d %H:%M:%f', snooze_until)
 WHERE typeof(snooze_until) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', snooze_until) IS NOT NULL
   AND snooze_until <> strftime('%Y-%m-%d %H:%M:%f', snooze_until);
UPDATE OR IGNORE tasks SET closed_at = strftime('%Y-%m-%d %H:%M:%f', closed_at)
 WHERE typeof(closed_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', closed_at) IS NOT NULL
   AND closed_at <> strftime('%Y-%m-%d %H:%M:%f', closed_at);
UPDATE OR IGNORE tasks SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE team_agents SET added_at = strftime('%Y-%m-%d %H:%M:%f', added_at)
 WHERE typeof(added_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', added_at) IS NOT NULL
   AND added_at <> strftime('%Y-%m-%d %H:%M:%f', added_at);

UPDATE OR IGNORE team_github_groups SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE team_github_repos SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);

UPDATE OR IGNORE team_settings SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE teams SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE teams SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);
UPDATE OR IGNORE teams SET deleted_at = strftime('%Y-%m-%d %H:%M:%f', deleted_at)
 WHERE typeof(deleted_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', deleted_at) IS NOT NULL
   AND deleted_at <> strftime('%Y-%m-%d %H:%M:%f', deleted_at);
UPDATE OR IGNORE teams SET shipped_defaults_backfilled_at = strftime('%Y-%m-%d %H:%M:%f', shipped_defaults_backfilled_at)
 WHERE typeof(shipped_defaults_backfilled_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', shipped_defaults_backfilled_at) IS NOT NULL
   AND shipped_defaults_backfilled_at <> strftime('%Y-%m-%d %H:%M:%f', shipped_defaults_backfilled_at);

UPDATE OR IGNORE user_github_identities SET verified_at = strftime('%Y-%m-%d %H:%M:%f', verified_at)
 WHERE typeof(verified_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', verified_at) IS NOT NULL
   AND verified_at <> strftime('%Y-%m-%d %H:%M:%f', verified_at);
UPDATE OR IGNORE user_github_identities SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE user_github_identities SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);
UPDATE OR IGNORE user_github_identities SET dashboard_backfilled_at = strftime('%Y-%m-%d %H:%M:%f', dashboard_backfilled_at)
 WHERE typeof(dashboard_backfilled_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', dashboard_backfilled_at) IS NOT NULL
   AND dashboard_backfilled_at <> strftime('%Y-%m-%d %H:%M:%f', dashboard_backfilled_at);

UPDATE OR IGNORE user_jira_identities SET verified_at = strftime('%Y-%m-%d %H:%M:%f', verified_at)
 WHERE typeof(verified_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', verified_at) IS NOT NULL
   AND verified_at <> strftime('%Y-%m-%d %H:%M:%f', verified_at);
UPDATE OR IGNORE user_jira_identities SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE user_jira_identities SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE user_settings SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

UPDATE OR IGNORE users SET created_at = strftime('%Y-%m-%d %H:%M:%f', created_at)
 WHERE typeof(created_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', created_at) IS NOT NULL
   AND created_at <> strftime('%Y-%m-%d %H:%M:%f', created_at);
UPDATE OR IGNORE users SET updated_at = strftime('%Y-%m-%d %H:%M:%f', updated_at)
 WHERE typeof(updated_at) = 'text'
   AND strftime('%Y-%m-%d %H:%M:%f', updated_at) IS NOT NULL
   AND updated_at <> strftime('%Y-%m-%d %H:%M:%f', updated_at);

-- +goose Down
SELECT 'down not supported';
