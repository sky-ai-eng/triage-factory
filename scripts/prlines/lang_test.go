package main

import "testing"

// scanned renders a file's kinds as a compact string so a case reads as the
// shape of the file: c=code, #=comment, .=blank.
func scanned(t *testing.T, l lang, src string) string {
	t.Helper()
	kinds := scanFile(l, []byte(src))
	out := make([]byte, len(kinds))
	for i, k := range kinds {
		switch k {
		case lineCode:
			out[i] = 'c'
		case lineComment:
			out[i] = '#'
		default:
			out[i] = '.'
		}
	}
	return string(out)
}

func TestScanGo(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "line comments and code",
			src:  "// why this exists\npackage main\n\nvar x = 1 // trailing commentary is still a code line\n",
			want: "#c.c",
		},
		{
			name: "block comment spans lines",
			src:  "/*\nMiddle lines look like nothing in particular.\n*/\nvar x = 1\n",
			want: "###c",
		},
		{
			name: "url in a string is not a comment",
			src:  "const u = \"https://example.com\"\n\t\"http://x\" +\n",
			want: "cc",
		},
		{
			name: "raw string content is code, not comment",
			src:  "var q = `\n// this is data, not a comment\n`\n",
			want: "ccc",
		},
		{
			name: "rune literal does not open a string",
			src:  "if c == '\\'' { return } // ok\nvar y = 2\n",
			want: "cc",
		},
		{
			name: "block comment closing mid-line leaves code",
			src:  "/* lead-in */ var z = 3\n",
			want: "c",
		},
		{
			name: "blank inside a block comment is blank",
			src:  "/* one\n\n   two */\n",
			want: "#.#",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanned(t, langGo, tc.src); got != tc.want {
				t.Errorf("scan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanJSX(t *testing.T) {
	// The multi-line {/* ... */} block is the case that forces whole-file
	// scanning: its middle lines carry no marker of their own.
	src := "export function A() {\n" +
		"  return (\n" +
		"    <div>\n" +
		"      {/* why we render nothing here:\n" +
		"          the parent already did */}\n" +
		"      <B url=\"https://x/y\" />\n" +
		"    </div>\n" +
		"  )\n" +
		"}\n"
	if got, want := scanned(t, langJS, src), "ccc##cccc"; got != want {
		t.Errorf("scan = %q, want %q", got, want)
	}
}

func TestScanJSApostrophe(t *testing.T) {
	// An unterminated single quote in prose must not swallow the line, and a
	// terminated one must still hide a // inside it.
	src := "<p>it's fine</p>\nconst a = 'https://x'\n"
	if got, want := scanned(t, langJS, src), "cc"; got != want {
		t.Errorf("scan = %q, want %q", got, want)
	}
}

func TestScanRust(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"lifetime tick is not a string", "fn f<'a>(s: &'a str) -> &'a str { s } // ok\n", "c"},
		{"nested block comments", "/* outer /* inner */ still outer */\nlet x = 1;\n", "#c"},
		{"raw string hides a comment marker", "let s = r#\"\n// data\n\"#;\n", "ccc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scanned(t, langRust, tc.src); got != tc.want {
				t.Errorf("scan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScanSQLAndHashAndMarkup(t *testing.T) {
	sql := "-- +goose Up\nCREATE TABLE t (id TEXT); -- inline\n/* block\n   two */\n"
	if got, want := scanned(t, langSQL, sql), "#c##"; got != want {
		t.Errorf("sql scan = %q, want %q", got, want)
	}

	sh := "#!/usr/bin/env bash\n# why\nset -euo pipefail\necho \"a # b\"\n"
	if got, want := scanned(t, langHash, sh), "##cc"; got != want {
		t.Errorf("hash scan = %q, want %q", got, want)
	}

	html := "<!-- note\n     continued -->\n<p>hi</p>\n"
	if got, want := scanned(t, langMarkup, html), "##c"; got != want {
		t.Errorf("markup scan = %q, want %q", got, want)
	}
}

func TestScanNoneTreatsEverythingAsCode(t *testing.T) {
	// Prompt blocks and JSON have no comment form; a leading // there is content.
	if got, want := scanned(t, langNone, "// not a comment here\n\ntext\n"), "c.c"; got != want {
		t.Errorf("scan = %q, want %q", got, want)
	}
}

func TestScanLineCountMatchesFile(t *testing.T) {
	// Line numbers from the diff index into this slice, so an off-by-one from a
	// trailing newline would silently shift every classification.
	if got := len(scanFile(langGo, []byte("a\nb\nc\n"))); got != 3 {
		t.Errorf("trailing newline: got %d lines, want 3", got)
	}
	if got := len(scanFile(langGo, []byte("a\nb\nc"))); got != 3 {
		t.Errorf("no trailing newline: got %d lines, want 3", got)
	}
}

func TestLangForContentDetectsShebang(t *testing.T) {
	if got := langForContent("internal/githooks/pre-push", []byte("#!/bin/sh\n# why\n")); got.name != langHash.name {
		t.Errorf("extensionless script resolved to %q, want hash", got.name)
	}
	if got := langForContent("data/blob", []byte("no shebang\n")); got.name != langNone.name {
		t.Errorf("plain data resolved to %q, want none", got.name)
	}
}

// TestCharLiteralLen asserts the byte length itself. The scanner tests above
// can only see a line's final classification, and hasCode is already set by the
// time this function returns — so a literal reported a byte short leaves no
// trace there, and the scanner walks on from the wrong offset unnoticed.
func TestCharLiteralLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{`'a'`, 3},
		{`'é'`, 4}, // one rune, two bytes
		{`'\n'`, 4},
		{`'\\'`, 4},
		{`'\''`, 4}, // the escaped quote is not the closer
		{`'\x41'`, 6},
		{`'\U0001F600'`, 12}, // longest escape Go has
		{`'\u{10FFFF}'`, 12}, // ...and Rust
		{`'a'b'`, 3},         // stops at the first real closer
		{`''`, 0},
		{`'a`, 0},
		{`'\'`, 0},       // the backslash escapes the would-be closer
		{`'a str`, 0},    // a Rust lifetime tick, not a literal
		{`'static x`, 0}, // ...and a longer one
		{`x'a'`, 0},      // must start at the quote
	}
	for _, tc := range cases {
		if got := charLiteralLen(tc.in); got != tc.want {
			t.Errorf("charLiteralLen(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestScanEscapedQuoteLiteralLeavesNoState checks that quote-dense lines end
// with the scanner back in the neutral state, since an unbalanced string or
// comment state is what would corrupt every line after this one — the damage a
// single line's own classification cannot show.
//
// Note this does NOT cover charLiteralLen's byte length: a literal consumed a
// byte short still lands here as code, because the stray quote is re-read as
// ordinary punctuation and hasCode is already set. That is what TestCharLiteralLen
// is for.
func TestScanEscapedQuoteLiteralLeavesNoState(t *testing.T) {
	var st scanState
	if got := scanLine(langGo, "q := '\\''+'x' // done", &st); got != lineCode {
		t.Errorf("got %v, want code", got)
	}
	if st.raw != "" || st.blockDepth != 0 {
		t.Errorf("scanner ended the line mid-state: raw=%q blockDepth=%d", st.raw, st.blockDepth)
	}
}

func TestClassifyTextFallback(t *testing.T) {
	cases := []struct {
		l    lang
		text string
		want lineKind
	}{
		{langGo, "   ", lineBlank},
		{langGo, "// why", lineComment},
		{langGo, " * continuation", lineComment},
		{langGo, "x := 1", lineCode},
		{langNone, "// content", lineCode},
	}
	for _, tc := range cases {
		if got := classifyText(tc.l, tc.text); got != tc.want {
			t.Errorf("classifyText(%s, %q) = %v, want %v", tc.l.name, tc.text, got, tc.want)
		}
	}
}
