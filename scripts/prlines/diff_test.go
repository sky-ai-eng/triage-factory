package main

import "testing"

func TestParseUnifiedDiffLineNumbers(t *testing.T) {
	raw := `diff --git a/a.go b/a.go
index 111..222 100644
--- a/a.go
+++ b/a.go
@@ -3,0 +4,2 @@ func f() {
+	x := 1
+	y := 2
@@ -10,1 +12,1 @@
-	old()
+	new()
`
	files, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	f := files[0]
	if f.OldPath != "a.go" || f.NewPath != "a.go" {
		t.Fatalf("paths = %q/%q", f.OldPath, f.NewPath)
	}
	wantAdded := []int{4, 5, 12}
	for i, cl := range f.Added {
		if cl.No != wantAdded[i] {
			t.Errorf("added[%d].No = %d, want %d", i, cl.No, wantAdded[i])
		}
	}
	if len(f.Removed) != 1 || f.Removed[0].No != 10 {
		t.Errorf("removed = %+v, want one line at 10", f.Removed)
	}
}

func TestParseUnifiedDiffDashContentIsNotAHeader(t *testing.T) {
	// A removed SQL comment renders as "----- …", and a removed markdown rule
	// as "----". Both are byte-identical to a file header until you know a hunk
	// has started.
	raw := `diff --git a/m.sql b/m.sql
--- a/m.sql
+++ b/m.sql
@@ -1,3 +1,1 @@
--- +goose Up
--- a comment
+SELECT 1;
`
	files, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	f := files[0]
	if f.OldPath != "m.sql" || f.NewPath != "m.sql" {
		t.Fatalf("hunk content overwrote the paths: %q/%q", f.OldPath, f.NewPath)
	}
	if len(f.Removed) != 2 {
		t.Fatalf("got %d removed lines, want 2: %+v", len(f.Removed), f.Removed)
	}
	if f.Removed[0].Text != "-- +goose Up" {
		t.Errorf("removed[0].Text = %q", f.Removed[0].Text)
	}
	if len(f.Added) != 1 || f.Added[0].Text != "SELECT 1;" {
		t.Errorf("added = %+v", f.Added)
	}
}

func TestParseUnifiedDiffAddDeleteRenameBinary(t *testing.T) {
	raw := `diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,1 @@
+package p
diff --git a/gone.go b/gone.go
deleted file mode 100644
--- a/gone.go
+++ /dev/null
@@ -1,1 +0,0 @@
-package p
diff --git a/old/name.go b/new/name.go
similarity index 92%
rename from old/name.go
rename to new/name.go
--- a/old/name.go
+++ b/new/name.go
@@ -1,1 +1,1 @@
-package old
+package new
diff --git a/logo.png b/logo.png
index 333..444 100644
Binary files a/logo.png and b/logo.png differ
`
	files, err := parseUnifiedDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4", len(files))
	}

	if files[0].OldPath != "" || files[0].NewPath != "new.go" || files[0].Path() != "new.go" {
		t.Errorf("added file = %+v", files[0])
	}
	if files[1].NewPath != "" || files[1].Path() != "gone.go" {
		t.Errorf("deleted file = %+v", files[1])
	}
	// A rename is classified by where the code ended up, not where it was.
	if files[2].Path() != "new/name.go" {
		t.Errorf("renamed file Path() = %q", files[2].Path())
	}
	if !files[3].Binary {
		t.Errorf("png not marked binary: %+v", files[3])
	}
}

func TestHeaderPathUnquotes(t *testing.T) {
	if got := headerPath(`"a/with space.go"`); got != "with space.go" {
		t.Errorf("got %q", got)
	}
	if got := headerPath("/dev/null"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseHunkHeaderOmittedCount(t *testing.T) {
	// git omits ",1" — a header of "@@ -7 +9 @@" means one line on each side.
	o, n, err := parseHunkHeader("@@ -7 +9 @@")
	if err != nil {
		t.Fatal(err)
	}
	if o != 7 || n != 9 {
		t.Errorf("got old=%d new=%d, want 7/9", o, n)
	}
	if _, _, err := parseHunkHeader("@@ nonsense @@"); err == nil {
		t.Error("expected an error on a malformed header")
	}
}
