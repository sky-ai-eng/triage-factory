package db

import (
	"database/sql"
	"testing"
)

func TestSQLiteSearch(t *testing.T) {
	clause, args := SQLiteSearch("  50%_Off ", "a.title", "b.body")
	want := `(tf_search_fold(a.title) LIKE ? ESCAPE '\' OR tf_search_fold(b.body) LIKE ? ESCAPE '\')`
	if clause != want {
		t.Errorf("clause = %s, want %s", clause, want)
	}
	if len(args) != 2 || args[0] != `%50\%\_off%` || args[1] != args[0] {
		t.Errorf("args = %#v", args)
	}
	if c, a := SQLiteSearch("   ", "a.title"); c != "" || a != nil {
		t.Errorf("blank needle rendered %q %v", c, a)
	}
}

// TestSQLiteSearch_FoldsNonASCII runs the predicate on SQLite itself: a
// needle typed with a capital outside A-Z matches the text it was typed from,
// and the other case of it, which SQLite's own LOWER would miss.
func TestSQLiteSearch_FoldsNonASCII(t *testing.T) {
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TABLE p (name TEXT); INSERT INTO p VALUES ('Überprüfung'), ('über alles'), ('50% off'), (NULL)`); err != nil {
		t.Fatal(err)
	}
	count := func(needle string) int {
		clause, args := SQLiteSearch(needle, "name")
		var n int
		if err := conn.QueryRow(`SELECT COUNT(*) FROM p WHERE `+clause, args...).Scan(&n); err != nil {
			t.Fatalf("search %q: %v", needle, err)
		}
		return n
	}
	for needle, want := range map[string]int{"Über": 2, "über": 2, "PRÜFUNG": 1, "50%": 1, "5_": 0} {
		if got := count(needle); got != want {
			t.Errorf("search %q matched %d rows, want %d", needle, got, want)
		}
	}
}

func TestPostgresSearch(t *testing.T) {
	clause, args := PostgresSearch("x", []any{"org"}, "a.title", "b.body")
	want := `(lower(a.title) LIKE $2 ESCAPE '\' OR lower(b.body) LIKE $2 ESCAPE '\')`
	if clause != want {
		t.Errorf("clause = %s, want %s", clause, want)
	}
	if len(args) != 2 || args[1] != "%x%" {
		t.Errorf("args = %#v", args)
	}
	if c, a := PostgresSearch("", []any{"org"}, "a.title"); c != "" || len(a) != 1 {
		t.Errorf("blank needle rendered %q %v", c, a)
	}
}
