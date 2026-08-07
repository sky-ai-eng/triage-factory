package jira

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// adfJSON renders a converted document the way the client would put it on the
// wire, so the golden strings below are literally what Jira receives.
func adfJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(wikiToADF(s))
	if err != nil {
		t.Fatalf("marshal adf: %v", err)
	}
	return string(b)
}

// TestWikiToADF_Blocks pins the block-level conversion: the constructs the
// shipped Jira-formatting guidance teaches, plus the fence dialects an agent
// writing Markdown into a Jira body actually produces.
func TestWikiToADF_Blocks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single paragraph",
			in:   "Deploy looks clean.",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Deploy looks clean."}]}]}`,
		},
		{
			name: "line break inside a paragraph is a hardBreak",
			in:   "first line\nsecond line",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"first line"},{"type":"hardBreak"},{"type":"text","text":"second line"}]}]}`,
		},
		{
			name: "blank line splits paragraphs",
			in:   "one\n\ntwo",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]},{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}`,
		},
		{
			name: "heading",
			in:   "h2. Findings\nbody",
			want: `{"type":"doc","version":1,"content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Findings"}]},{"type":"paragraph","content":[{"type":"text","text":"body"}]}]}`,
		},
		{
			name: "wiki code block with language",
			in:   "{code:go}\nfmt.Println(\"x\")\n{code}",
			want: `{"type":"doc","version":1,"content":[{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"fmt.Println(\"x\")"}]}]}`,
		},
		{
			name: "wiki code block with named parameters",
			in:   "{code:title=Example|language=java}\nint x;\n{code}",
			want: `{"type":"doc","version":1,"content":[{"type":"codeBlock","attrs":{"language":"java"},"content":[{"type":"text","text":"int x;"}]}]}`,
		},
		{
			name: "noformat block",
			in:   "{noformat}\nraw *text*\n{noformat}",
			want: `{"type":"doc","version":1,"content":[{"type":"codeBlock","content":[{"type":"text","text":"raw *text*"}]}]}`,
		},
		{
			name: "markdown fence",
			in:   "```sh\nmake test\n```",
			want: `{"type":"doc","version":1,"content":[{"type":"codeBlock","attrs":{"language":"sh"},"content":[{"type":"text","text":"make test"}]}]}`,
		},
		{
			name: "unterminated code block keeps its text",
			in:   "{code}\nstill mine",
			want: `{"type":"doc","version":1,"content":[{"type":"codeBlock","content":[{"type":"text","text":"still mine"}]}]}`,
		},
		{
			name: "quote block",
			in:   "{quote}\nthey said\n{quote}",
			want: `{"type":"doc","version":1,"content":[{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"they said"}]}]}]}`,
		},
		{
			name: "bq line",
			in:   "bq. short quote",
			want: `{"type":"doc","version":1,"content":[{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"short quote"}]}]}]}`,
		},
		{
			name: "bullet list",
			in:   "* one\n* two",
			want: `{"type":"doc","version":1,"content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}]}]}`,
		},
		{
			name: "ordered list",
			in:   "# first\n# second",
			want: `{"type":"doc","version":1,"content":[{"type":"orderedList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}]}]}`,
		},
		{
			name: "nested bullet list",
			in:   "* outer\n** inner\n* back",
			want: `{"type":"doc","version":1,"content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"outer"}]},{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"inner"}]}]}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"back"}]}]}]}]}`,
		},
		{
			name: "kind switch at one depth becomes sibling lists",
			in:   "* bullet\n# number",
			want: `{"type":"doc","version":1,"content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"bullet"}]}]}]},{"type":"orderedList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"number"}]}]}]}]}`,
		},
		{
			name: "horizontal rule",
			in:   "above\n----\nbelow",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"above"}]},{"type":"rule"},{"type":"paragraph","content":[{"type":"text","text":"below"}]}]}`,
		},
		{
			name: "whitespace-only body still yields a valid document",
			in:   "   \n\t",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := adfJSON(t, tt.in); got != tt.want {
				t.Errorf("wikiToADF(%q):\n got %s\nwant %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestWikiToADF_Inline pins the inline marks and, just as importantly, the
// cases that must NOT be read as markup — the delimiter characters are ordinary
// punctuation in the identifiers and issue keys a triage comment is full of,
// and a false positive would delete them from someone's comment.
func TestWikiToADF_Inline(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strong",
			in:   "this is *bold* here",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"this is "},{"type":"text","text":"bold","marks":[{"type":"strong"}]},{"type":"text","text":" here"}]}]}`,
		},
		{
			name: "emphasis",
			in:   "an _aside_ note",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"an "},{"type":"text","text":"aside","marks":[{"type":"em"}]},{"type":"text","text":" note"}]}]}`,
		},
		{
			name: "monospace",
			in:   "call {{doRequest}} first",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"call "},{"type":"text","text":"doRequest","marks":[{"type":"code"}]},{"type":"text","text":" first"}]}]}`,
		},
		{
			name: "nested marks accumulate",
			in:   "*_both_*",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"both","marks":[{"type":"em"},{"type":"strong"}]}]}]}`,
		},
		{
			name: "aliased link",
			in:   "see [the PR|https://example.com/pr/1] for detail",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"see "},{"type":"text","text":"the PR","marks":[{"type":"link","attrs":{"href":"https://example.com/pr/1"}}]},{"type":"text","text":" for detail"}]}]}`,
		},
		{
			name: "bare link",
			in:   "[https://example.com]",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"https://example.com","marks":[{"type":"link","attrs":{"href":"https://example.com"}}]}]}]}`,
		},
		{
			name: "issue key in brackets is not a link",
			in:   "blocked by [SKY-123]",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"blocked by [SKY-123]"}]}]}`,
		},
		{
			name: "snake_case is not emphasis",
			in:   "the max_retry_count field",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"the max_retry_count field"}]}]}`,
		},
		{
			name: "lone asterisks stay literal",
			in:   "a * b * c is not markup because of spacing",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"a * b * c is not markup because of spacing"}]}]}`,
		},
		{
			name: "unclosed delimiter stays literal",
			in:   "5 * 3 items",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"5 * 3 items"}]}]}`,
		},
		{
			name: "markdown bold opening a line is not a nested bullet",
			in:   "**Summary** of the change",
			want: `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"*"},{"type":"text","text":"Summary","marks":[{"type":"strong"}]},{"type":"text","text":"* of the change"}]}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := adfJSON(t, tt.in); got != tt.want {
				t.Errorf("wikiToADF(%q):\n got %s\nwant %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestWikiToADF_KeepsProse is the guarantee that matters most: styling may be
// lost on any construct the converter does not recognize, but prose may not.
// Each sample is re-read through the ADF text extractor the read path already
// uses, and the words a reader would expect to still see must be there —
// including in the deliberately malformed sample, where nothing parses.
func TestWikiToADF_KeepsProse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		survive []string
	}{
		{
			name:    "mixed document",
			in:      "h3. Root cause\nThe *retry* loop in {{doRequest}} drops _the_ deadline.\n\n# Reproduce\n# Confirm\n\n{code:go}\nfor i := range xs {}\n{code}\nSee [the PR|https://example.com/pr/1].",
			survive: []string{"Root cause", "retry", "doRequest", "deadline", "Reproduce", "Confirm", "for i := range xs {}", "the PR"},
		},
		{
			name:    "malformed markup parses as nothing and loses nothing",
			in:      "weird ** unbalanced * markers _ everywhere ]] {{ and {code without a close",
			survive: []string{"weird", "unbalanced", "markers", "everywhere", "and", "without a close"},
		},
		{
			name:    "list depths and a kind switch",
			in:      "* one\n** two\n*** three\n# switched\nplain tail",
			survive: []string{"one", "two", "three", "switched", "plain tail"},
		},
		{
			name:    "non-ascii prose",
			in:      "unicode: café naïve — em-dash, and an ellipsis…",
			survive: []string{"café naïve — em-dash, and an ellipsis…"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(wikiToADF(tt.in))
			if err != nil {
				t.Fatalf("marshal adf: %v", err)
			}
			got := ExtractDescriptionText(raw)
			for _, want := range tt.survive {
				if !strings.Contains(got, want) {
					t.Errorf("extracted text %q lost %q", got, want)
				}
			}
		})
	}
}

// TestRichTextValue pins the version split at the seam every rich-text write
// goes through: v2 ships the wiki-markup string untouched, v3 ships a document,
// and an empty value clears the field rather than sending an empty document
// (which is not valid ADF).
func TestRichTextValue(t *testing.T) {
	dc := DataCenterPAT("https://jira.example.com", "tok")
	if got := dc.richTextValue("h2. Title"); got != "h2. Title" {
		t.Errorf("v2 richTextValue = %#v, want the string verbatim", got)
	}
	if got := dc.richTextValue(""); got != "" {
		t.Errorf("v2 richTextValue(\"\") = %#v, want the empty string", got)
	}

	cloud := CloudAPIToken("https://acme.atlassian.net", "me@acme.com", "tok")
	doc, ok := cloud.richTextValue("h2. Title").(adfDoc)
	if !ok {
		t.Fatalf("v3 richTextValue = %T, want adfDoc", cloud.richTextValue("h2. Title"))
	}
	if doc.Type != "doc" || doc.Version != 1 {
		t.Errorf("v3 doc root = {%q, %d}, want {\"doc\", 1}", doc.Type, doc.Version)
	}
	if got := cloud.richTextValue("  "); got != nil {
		t.Errorf("v3 richTextValue(blank) = %#v, want nil so the field clears", got)
	}
}

// bodyCapture records the request path and raw body a write reached the server
// with. It is mutex-guarded so the test goroutine's read is race-free against
// the handler's write.
type bodyCapture struct {
	mu   sync.Mutex
	path string
	body string
}

func (b *bodyCapture) read() (path, body string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.path, b.body
}

// writeCaptureServer replies to any request with reply and records what it saw.
func writeCaptureServer(t *testing.T, reply string) (*httptest.Server, *bodyCapture) {
	t.Helper()
	rec := &bodyCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.path, rec.body = r.URL.Path, string(raw)
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestAddComment_WireShape is the acceptance check for both deployments: a Data
// Center comment still posts the plain wiki-markup string to v2, and a Cloud
// comment posts an ADF document to v3 — the shape v3 requires and the string
// form it rejects.
func TestAddComment_WireShape(t *testing.T) {
	t.Run("data center posts a v2 string", func(t *testing.T) {
		srv, rec := writeCaptureServer(t, `{"id":"101"}`)
		c := NewClient(DataCenterPAT(srv.URL, "pat"))
		if _, err := c.AddComment(context.Background(), "SKY-1", "h2. Done\n*shipped*"); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		path, body := rec.read()
		if want := "/rest/api/2/issue/SKY-1/comment"; path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		if want := `{"body":"h2. Done\n*shipped*"}`; body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
	})

	t.Run("cloud posts a v3 document", func(t *testing.T) {
		srv, rec := writeCaptureServer(t, `{"id":"101"}`)
		c := NewClient(CloudAPIToken(srv.URL, "me@acme.com", "tok"))
		if _, err := c.AddComment(context.Background(), "SKY-1", "h2. Done\n*shipped*"); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		path, body := rec.read()
		if want := "/rest/api/3/issue/SKY-1/comment"; path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		want := `{"body":{"type":"doc","version":1,"content":[` +
			`{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Done"}]},` +
			`{"type":"paragraph","content":[{"type":"text","text":"shipped","marks":[{"type":"strong"}]}]}]}}`
		if body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
	})

	t.Run("proxy client follows the relayed deployment", func(t *testing.T) {
		srv, rec := writeCaptureServer(t, `{"id":"101"}`)
		c := NewClient(ProxyPlaceholder(srv.URL, "per-run-placeholder", DeploymentCloud))
		if _, err := c.AddComment(context.Background(), "SKY-1", "ok"); err != nil {
			t.Fatalf("AddComment: %v", err)
		}
		path, body := rec.read()
		if want := "/rest/api/3/issue/SKY-1/comment"; path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		want := `{"body":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"ok"}]}]}}`
		if body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
	})
}

// TestIssueDescription_WireShape covers the other rich-text write: a
// description is ADF on Cloud too, and clearing one sends null rather than an
// empty document, which is not valid ADF.
func TestIssueDescription_WireShape(t *testing.T) {
	t.Run("create sends a cloud description as a document", func(t *testing.T) {
		srv, rec := writeCaptureServer(t, `{"key":"SKY-2"}`)
		c := NewClient(CloudAPIToken(srv.URL, "me@acme.com", "tok"))
		if _, err := c.CreateIssue(context.Background(), "SKY", "Task", "Summary", "details", "", ""); err != nil {
			t.Fatalf("CreateIssue: %v", err)
		}
		_, body := rec.read()
		if !strings.Contains(body, `"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"details"}]}]}`) {
			t.Errorf("body = %s, want an ADF description", body)
		}
	})

	t.Run("update clears a cloud description with null", func(t *testing.T) {
		srv, rec := writeCaptureServer(t, `{}`)
		c := NewClient(CloudAPIToken(srv.URL, "me@acme.com", "tok"))
		empty := ""
		if err := c.UpdateIssue(context.Background(), "SKY-2", UpdateIssueFields{Description: &empty}); err != nil {
			t.Fatalf("UpdateIssue: %v", err)
		}
		_, body := rec.read()
		if want := `{"fields":{"description":null}}`; body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
	})

	t.Run("data center description is unchanged", func(t *testing.T) {
		srv, rec := writeCaptureServer(t, `{}`)
		c := NewClient(DataCenterPAT(srv.URL, "pat"))
		desc := "h2. Still wiki markup"
		if err := c.UpdateIssue(context.Background(), "SKY-2", UpdateIssueFields{Description: &desc}); err != nil {
			t.Fatalf("UpdateIssue: %v", err)
		}
		_, body := rec.read()
		if want := `{"fields":{"description":"h2. Still wiki markup"}}`; body != want {
			t.Errorf("body = %s, want %s", body, want)
		}
	})
}

// TestWikiToADF_BlockquoteStaysSchemaValid pins the narrower node set ADF
// permits inside a blockquote. Jira rejects the entire write on a schema
// violation, so a quoted heading must degrade to a paragraph rather than
// travel as a heading and fail the comment.
func TestWikiToADF_BlockquoteStaysSchemaValid(t *testing.T) {
	got := adfJSON(t, "{quote}\nh2. Quoted heading\n----\n* item\n{quote}")
	want := `{"type":"doc","version":1,"content":[{"type":"blockquote","content":[` +
		`{"type":"paragraph","content":[{"type":"text","text":"Quoted heading"}]},` +
		`{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item"}]}]}]}]}]}`
	if got != want {
		t.Errorf("blockquote interior:\n got %s\nwant %s", got, want)
	}
}
