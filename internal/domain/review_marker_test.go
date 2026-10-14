package domain

import "testing"

func TestReviewMarker_After(t *testing.T) {
	m := ReviewMarker{ID: 500, SubmittedAt: "2026-10-01T10:00:00Z"}
	cases := []struct {
		name        string
		submittedAt string
		id          int64
		want        bool
	}{
		{"later submission", "2026-10-01T10:00:01Z", 1, true},
		{"earlier submission, higher id", "2026-10-01T09:59:59Z", 900, false},
		{"same time, higher id", "2026-10-01T10:00:00Z", 501, true},
		{"same time, same id", "2026-10-01T10:00:00Z", 500, false},
		{"same moment in another offset", "2026-10-01T12:00:00+02:00", 499, false},
		{"unparseable time", "", 900, false},
	}
	for _, tc := range cases {
		if got := m.After(tc.submittedAt, tc.id); got != tc.want {
			t.Errorf("%s: After(%q, %d) = %v, want %v", tc.name, tc.submittedAt, tc.id, got, tc.want)
		}
	}
	if !(ReviewMarker{}).After("2026-10-01T10:00:00Z", 1) {
		t.Error("a zero marker: any submitted review should be newer")
	}
}

func TestReviewMarker_Newer(t *testing.T) {
	a := ReviewMarker{ID: 1, SubmittedAt: "2026-10-01T10:00:00Z"}
	b := ReviewMarker{ID: 2, SubmittedAt: "2026-10-01T11:00:00Z"}
	if a.Newer(b) != b || b.Newer(a) != b {
		t.Errorf("Newer did not pick b: %+v, %+v", a.Newer(b), b.Newer(a))
	}
	if (ReviewMarker{}).Newer(a) != a {
		t.Error("a zero marker should give way to any review")
	}
}

func TestCommentMarker_Newer(t *testing.T) {
	a := CommentMarker{ID: 1, CreatedAt: "2026-10-01T10:00:00Z"}
	b := CommentMarker{ID: 2, CreatedAt: "2026-10-01T10:00:00Z"}
	if a.Newer(b) != b || b.Newer(a) != b {
		t.Errorf("Newer did not pick the higher id at an equal time: %+v, %+v", a.Newer(b), b.Newer(a))
	}
	if (CommentMarker{}).Newer(a) != a {
		t.Error("a zero marker should give way to any comment")
	}
	if a.After("2026-10-01T09:59:59Z", 9) {
		t.Error("an earlier comment with a higher id was taken as newer")
	}
}
