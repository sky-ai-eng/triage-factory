package db

import "testing"

func TestSQLiteSearch(t *testing.T) {
	clause, args := SQLiteSearch("  50%_Off ", "a.title", "b.body")
	want := `(LOWER(a.title) LIKE ? ESCAPE '\' OR LOWER(b.body) LIKE ? ESCAPE '\')`
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
