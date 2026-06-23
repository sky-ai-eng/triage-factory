package pgtest

import (
	"database/sql"
	"testing"
)

// TestRLS_UserCanWriteTeam_RoleSemantics pins the helper that powers the
// viewer-role read-only enforcement (TFAC-447): tf.user_can_write_team is true
// for admins and members, false for viewers and non-members — while
// tf.user_in_team (the read gate) stays true for every member regardless of
// role. The split between the two is the whole mechanism: reads check
// membership, writes check role.
func TestRLS_UserCanWriteTeam_RoleSemantics(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	// alice founds the org as team admin of teamA.
	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	// bob is a plain member of teamA; carol is a viewer of teamA.
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")
	carol := SeedUser(t, h, "carol")
	AddOrgMember(t, h, carol, orgA, teamA, "member", "viewer")
	// dave is in the org but on a different team (teamB) — a non-member of teamA.
	teamB := SeedTeam(t, h, orgA, "teamB")
	dave := SeedUser(t, h, "dave")
	AddOrgMember(t, h, dave, orgA, teamB, "member", "admin")

	cases := []struct {
		name     string
		userID   string
		canWrite bool // expected tf.user_can_write_team(teamA)
		inTeam   bool // expected tf.user_in_team(teamA)
	}{
		{"admin", alice, true, true},
		{"member", bob, true, true},
		{"viewer", carol, false, true}, // viewer reads (in_team) but can't write
		{"non-member", dave, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := h.WithUser(t, tc.userID, orgA, func(tx *sql.Tx) error {
				var canWrite, inTeam bool
				if err := tx.QueryRow(
					`SELECT tf.user_can_write_team($1::uuid), tf.user_in_team($1::uuid)`, teamA,
				).Scan(&canWrite, &inTeam); err != nil {
					return err
				}
				if canWrite != tc.canWrite {
					t.Errorf("user_can_write_team = %v, want %v", canWrite, tc.canWrite)
				}
				if inTeam != tc.inTeam {
					t.Errorf("user_in_team = %v, want %v", inTeam, tc.inTeam)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("WithUser(%s): %v", tc.name, err)
			}
		})
	}
}

// TestRLS_ViewerCannotWriteTeamScopedPrompts is the end-to-end proof that the
// policy swap (user_in_team → user_can_write_team on the write paths) makes a
// viewer genuinely read-only: a viewer SELECTs a team prompt fine but every
// write — INSERT, UPDATE, DELETE — trips the RLS policy (42501). A plain member
// of the same team writes without issue, proving the gate keys on role, not
// membership.
func TestRLS_ViewerCannotWriteTeamScopedPrompts(t *testing.T) {
	h := Shared(t)
	h.Reset(t)

	orgA, alice, teamA := SeedOrgWithUser(t, h, "alice")
	bob := SeedUser(t, h, "bob")
	AddOrgMember(t, h, bob, orgA, teamA, "member", "member")
	carol := SeedUser(t, h, "carol")
	AddOrgMember(t, h, carol, orgA, teamA, "member", "viewer")

	// Seed a team prompt via the admin pool (RLS-bypassed fixture).
	var promptID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
		VALUES ($1, $2, $3, 'team-prompt', 'mission', 'user') RETURNING id
	`, orgA, alice, teamA).Scan(&promptID); err != nil {
		t.Fatalf("seed prompt: %v", err)
	}

	// Viewer can READ the team's prompt.
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM prompts WHERE id = $1`, promptID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("viewer saw %d prompts, want 1 (reads must succeed)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("viewer read: %v", err)
	}

	// Viewer INSERT of a new team prompt → RLS rejects.
	err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
			VALUES ($1, $2, $3, 'viewer-prompt', 'mission', 'user')
		`, orgA, carol, teamA)
		return e
	})
	AssertRLSViolation(t, err)

	// Viewer UPDATE of the existing prompt → RLS rejects (zero-row UPDATEs don't
	// raise; this fails the USING clause via the WITH CHECK path on a real
	// column change, but a viewer can't even see it as updatable — the UPDATE
	// affects no rows, so assert that instead of an error here).
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, e := tx.Exec(`UPDATE prompts SET body = 'tampered' WHERE id = $1`, promptID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("viewer UPDATE affected %d rows, want 0 (RLS USING must hide the row from writes)", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("viewer update: %v", err)
	}

	// Viewer DELETE → same: zero rows, the row isn't write-visible.
	if err := h.WithUser(t, carol, orgA, func(tx *sql.Tx) error {
		res, e := tx.Exec(`DELETE FROM prompts WHERE id = $1`, promptID)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 0 {
			t.Errorf("viewer DELETE affected %d rows, want 0", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("viewer delete: %v", err)
	}

	// Member of the same team CAN write — proves the gate is role-keyed, not a
	// blanket team lockout.
	if err := h.WithUser(t, bob, orgA, func(tx *sql.Tx) error {
		_, e := tx.Exec(`
			INSERT INTO prompts (org_id, creator_user_id, team_id, name, body, source)
			VALUES ($1, $2, $3, 'member-prompt', 'mission', 'user')
		`, orgA, bob, teamA)
		return e
	}); err != nil {
		t.Fatalf("member INSERT should succeed, got: %v", err)
	}

	// Prompt still present + untampered after the viewer's blocked writes.
	var body string
	if err := h.AdminDB.QueryRow(`SELECT body FROM prompts WHERE id = $1`, promptID).Scan(&body); err != nil {
		t.Fatalf("re-read prompt: %v", err)
	}
	if body != "mission" {
		t.Errorf("prompt body = %q, want %q (viewer write must not have landed)", body, "mission")
	}
}
