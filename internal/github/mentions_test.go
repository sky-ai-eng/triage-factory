package github

import (
	"slices"
	"testing"
)

func TestMentions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{name: "empty", body: "", want: []string{}},
		{name: "a user", body: "@alice can you look", want: []string{"alice"}},
		{name: "in lower case, once each, in order", body: "cc @Bob, @alice and @BOB", want: []string{"bob", "alice"}},
		{name: "a team", body: "ping @Acme/Back_end-team", want: []string{"acme/back_end-team"}},
		{name: "a slug's trailing hyphens", body: "@acme/infra-- now", want: []string{"acme/infra"}},
		{name: "a login's trailing hyphens", body: "@hank- thoughts? @erin--, @bob-.", want: []string{"hank", "erin", "bob"}},
		{name: "a hyphen inside a login", body: "@hank-hill", want: []string{"hank-hill"}},
		{name: "a slash with no slug", body: "@acme/ and @acme/-x", want: []string{}},
		{name: "punctuation after a login", body: "(@alice), @bob's, @carol.", want: []string{"alice", "bob", "carol"}},
		{name: "dots then a space", body: "thanks @alice... really", want: []string{"alice"}},
		{name: "a dotted or underscored name", body: "@alice.bob and @carol_d", want: []string{}},
		{name: "after a word character", body: "mail a@b.com or x_@alice", want: []string{}},
		{name: "a login must start with a letter or digit", body: "@-alice @_bob", want: []string{}},
		{name: "after markup", body: "**@alice** _@bob_ __@dan__ [@carol]", want: []string{"alice", "bob", "dan", "carol"}},
		{name: "inline code", body: "`@alice` and @bob", want: []string{"bob"}},
		{name: "a double-backtick span", body: "``a ` @alice`` @bob", want: []string{"bob"}},
		{name: "an unclosed backtick is text", body: "a ` @alice", want: []string{"alice"}},
		{name: "text right after inline code", body: "`x`@alice", want: []string{"alice"}},
		{name: "a fenced block", body: "```go\n@alice\n```\n@bob", want: []string{"bob"}},
		{name: "a fence closes on a run as long", body: "~~~~\n@alice\n~~~\n@bob\n~~~~~\n@carol", want: []string{"carol"}},
		{name: "an unclosed fence runs to the end", body: "```\n@alice", want: []string{}},
		{name: "an indented fence in a list", body: "- item\n  ```\n  @alice\n  ```\n- @bob", want: []string{"bob"}},
		{name: "backticks in an info string are inline code", body: "```a` @alice", want: []string{"alice"}},
		{name: "an indented code block", body: "text\n\n    @alice\n\t@bob\n@carol", want: []string{"carol"}},
		{name: "an indented paragraph continuation", body: "text\n    @alice", want: []string{"alice"}},
		{name: "an indented line after a heading", body: "# Title @bob\n    @alice", want: []string{"bob"}},
		{name: "a quoted line", body: "> @alice wrote this\n   > @carol too\n@bob", want: []string{"bob"}},
		{name: "an HTML comment", body: "<!-- @alice -->@bob", want: []string{"bob"}},
		{name: "a multi-line HTML comment", body: "<!--\ncc @alice\n```\n-->\n@carol", want: []string{"carol"}},
		{name: "a URL", body: "see https://medium.com/@alice/post or HTTP://x.io/@dan and @bob", want: []string{"bob"}},
		{name: "a link to a profile names the user in its text", body: "[@alice](https://github.com/alice)", want: []string{"alice"}},
		{name: "CRLF lines", body: "@alice\r\n```\r\n@bob\r\n```\r\n@carol", want: []string{"alice", "carol"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Mentions(tc.body)
			if got == nil {
				t.Fatal("Mentions returned nil, want a non-nil slice")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Mentions(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestBuildSnapshot_BodyMentions pins that both snapshot builders record the
// body's mentions, an empty set for a body that mentions nobody, and nil for a
// response without the body, which is what tells the diff the set is unknown.
func TestBuildSnapshot_BodyMentions(t *testing.T) {
	gql := func(body string) gqlPR {
		pr := gqlPR{Number: 1}
		if body != "" {
			pr.Body = []byte(body)
		}
		return pr
	}
	rest := func(body string) restPR {
		pr := restPR{Number: 1}
		if body != "" {
			pr.Body = []byte(body)
		}
		return pr
	}
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{name: "mentions", raw: `"Fixes it. cc @Alice and @acme/backend"`, want: []string{"alice", "acme/backend"}},
		{name: "nobody", raw: `"Fixes it."`, want: []string{}},
		{name: "a cleared body", raw: `null`, want: []string{}},
		{name: "no body in the response", raw: "", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for builder, got := range map[string][]string{
				"graphql full":      gql(tc.raw).toSnapshot().BodyMentions,
				"graphql discovery": gql(tc.raw).toDiscoverySnapshot().BodyMentions,
				"rest":              rest(tc.raw).toDiscoverySnapshot("acme", "repo").BodyMentions,
			} {
				if (got == nil) != (tc.want == nil) || !slices.Equal(got, tc.want) {
					t.Errorf("%s: BodyMentions = %#v, want %#v", builder, got, tc.want)
				}
			}
		})
	}
}
