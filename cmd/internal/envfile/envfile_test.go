package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpsert_CollapsesDuplicates: a file with stale duplicate lines collapses
// to one in-place line for the key, and unrelated lines survive.
func TestUpsert_CollapsesDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	seed := "A=1\nGOTRUE_JWT_KEYS=old1\nB=2\nGOTRUE_JWT_KEYS=old2\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Upsert(path, []Var{{"GOTRUE_JWT_KEYS", "new"}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := "A=1\nGOTRUE_JWT_KEYS=new\nB=2\n"; string(got) != want {
		t.Errorf("file = %q; want %q", got, want)
	}
}

// TestUpsert_PreservesExportPrefix: an `export KEY=` line keeps its prefix when
// its value is replaced, so a sourced .env keeps exporting it.
func TestUpsert_PreservesExportPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("export GOTRUE_JWT_KEYS=old\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Upsert(path, []Var{{"GOTRUE_JWT_KEYS", "new"}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "export GOTRUE_JWT_KEYS=new") {
		t.Errorf("export prefix not preserved; got:\n%s", b)
	}
}

// TestUpsert_CreatesAt0600: a missing file is created, and an existing file of
// any mode ends up 0600, because what is in it is secret.
func TestUpsert_CreatesAt0600(t *testing.T) {
	dir := t.TempDir()
	created := filepath.Join(dir, "new.env")
	if err := Upsert(created, []Var{{"A", "1"}}); err != nil {
		t.Fatalf("Upsert(new): %v", err)
	}
	loose := filepath.Join(dir, "loose.env")
	if err := os.WriteFile(loose, []byte("A=0\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Upsert(loose, []Var{{"A", "1"}}); err != nil {
		t.Fatalf("Upsert(loose): %v", err)
	}
	for _, p := range []string{created, loose} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %o; want 600", filepath.Base(p), mode)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("dir holds %d entries; want 2 (a temp file was left behind)", len(entries))
	}
}

// TestSet_AppendsInGivenOrderAndFillsEmptyLines: an empty `KEY=` line (the
// shape .env.example ships) is filled where it stands, and keys the content
// never assigns are appended in the order given. A commented-out assignment is
// a comment, not an assignment.
func TestSet_AppendsInGivenOrderAndFillsEmptyLines(t *testing.T) {
	content := []byte("# note\nA=\n# B=example\n")
	got := Set(content, []Var{{"C", "3"}, {"A", "1"}, {"B", "2"}})
	want := "# note\nA=1\n# B=example\nC=3\nB=2\n"
	if string(got) != want {
		t.Errorf("Set = %q; want %q", got, want)
	}
}

func TestLookup(t *testing.T) {
	content := []byte("A=1\n# B=commented\nexport C = \"quoted\" \nD=\nA=2\n")
	tests := []struct {
		key       string
		want      string
		wantFound bool
	}{
		{"A", "2", true},
		{"B", "", false},
		{"C", "quoted", true},
		{"D", "", true},
		{"E", "", false},
	}
	for _, tt := range tests {
		got, found := Lookup(content, tt.key)
		if got != tt.want || found != tt.wantFound {
			t.Errorf("Lookup(%s) = %q, %v; want %q, %v", tt.key, got, found, tt.want, tt.wantFound)
		}
	}
}
