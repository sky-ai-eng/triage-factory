package dbtest

import (
	"database/sql"
	"testing"
	"time"
)

// AgeNotification moves every stamp on a notification and on its marks d
// earlier, through raw SQLite SQL, as if the writes so far had happened d
// ago. The stores stamp these columns with the clock, so a test that orders a
// write after earlier ones ages the earlier ones rather than waiting for the
// clock to move. expires_at stays, so a notice ages without expiring.
func AgeNotification(tb testing.TB, database *sql.DB, id string, d time.Duration) {
	tb.Helper()
	var first, last, escalated time.Time
	var resolved sql.NullTime
	if err := database.QueryRow(`
		SELECT first_seen_at, last_seen_at, escalated_at, resolved_at FROM notifications WHERE id = ?`, id).
		Scan(&first, &last, &escalated, &resolved); err != nil {
		tb.Fatalf("AgeNotification %s: %v", id, err)
	}
	if _, err := database.Exec(`
		UPDATE notifications SET first_seen_at = ?, last_seen_at = ?, escalated_at = ?, resolved_at = ? WHERE id = ?`,
		first.Add(-d).UTC(), last.Add(-d).UTC(), escalated.Add(-d).UTC(), agedNullTime(resolved, d), id); err != nil {
		tb.Fatalf("AgeNotification %s: %v", id, err)
	}

	type marks struct {
		userID              string
		readAt, dismissedAt sql.NullTime
	}
	rows, err := database.Query(`SELECT user_id, read_at, dismissed_at FROM notification_marks WHERE notification_id = ?`, id)
	if err != nil {
		tb.Fatalf("AgeNotification %s marks: %v", id, err)
	}
	var all []marks
	for rows.Next() {
		var m marks
		if err := rows.Scan(&m.userID, &m.readAt, &m.dismissedAt); err != nil {
			rows.Close()
			tb.Fatalf("AgeNotification %s marks: %v", id, err)
		}
		all = append(all, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		tb.Fatalf("AgeNotification %s marks: %v", id, err)
	}
	for _, m := range all {
		if _, err := database.Exec(`
			UPDATE notification_marks SET read_at = ?, dismissed_at = ? WHERE notification_id = ? AND user_id = ?`,
			agedNullTime(m.readAt, d), agedNullTime(m.dismissedAt, d), id, m.userID); err != nil {
			tb.Fatalf("AgeNotification %s marks: %v", id, err)
		}
	}
}

func agedNullTime(t sql.NullTime, d time.Duration) any {
	if !t.Valid {
		return nil
	}
	return t.Time.Add(-d).UTC()
}
