package commitsign

import (
	"errors"
	"strings"
	"testing"
)

const (
	testTree   = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	testParent = "1111111111111111111111111111111111111111"
)

func TestParseCommitter(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    Identity
	}{
		{
			name:    "root commit",
			payload: testPayload,
			want:    Identity{Name: "Octo Cat", Email: "1+octocat@users.noreply.github.com"},
		},
		{
			name: "merge with a mergetag, whose continuation lines are not headers",
			payload: "tree " + testTree + "\n" +
				"parent " + testParent + "\n" +
				"parent 2222222222222222222222222222222222222222\n" +
				"author Someone Else <else@example.com> 1600000000 -0700\n" +
				"committer Triage Factory <9+tf@users.noreply.github.com> 1700000000 +0100\n" +
				"mergetag object 2222222222222222222222222222222222222222\n" +
				" type commit\n" +
				" tag v1\n" +
				" committer Mallory <mallory@example.com> 1 +0000\n" +
				"\n" +
				"Merge tag 'v1'\n\ncommitter Mallory <mallory@example.com> 1 +0000\n",
			want: Identity{Name: "Triage Factory", Email: "9+tf@users.noreply.github.com"},
		},
		{
			name:    "empty message",
			payload: strings.TrimSuffix(testPayload, "subject\n"),
			want:    Identity{Name: "Octo Cat", Email: "1+octocat@users.noreply.github.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCommitter([]byte(tc.payload))
			if err != nil {
				t.Fatalf("ParseCommitter: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseCommitter = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseCommitterRefuses(t *testing.T) {
	const committer = "committer Octo Cat <octocat@example.com> 1700000000 +0000\n"
	cases := []struct {
		name    string
		payload string
		object  string
	}{
		{name: "empty", payload: ""},
		{name: "no blank line", payload: "tree " + testTree + "\n" + committer},
		{name: "leading blank line", payload: "\ntree " + testTree + "\n" + committer + "\nmsg\n"},
		{name: "no tree", payload: committer + "\nmsg\n"},
		{name: "tree not first", payload: "parent " + testParent + "\ntree " + testTree + "\n" + committer + "\nmsg\n"},
		{name: "two trees", payload: "tree " + testTree + "\ntree " + testTree + "\n" + committer + "\nmsg\n"},
		{name: "SHA-256 tree", payload: "tree " + strings.Repeat("a", 64) + "\n" + committer + "\nmsg\n"},
		{name: "no committer", payload: "tree " + testTree + "\nauthor Octo Cat <octocat@example.com> 1700000000 +0000\n\nmsg\n"},
		{name: "committer only in the message", payload: "tree " + testTree + "\n\n" + committer},
		{name: "two committers", payload: "tree " + testTree + "\n" + committer + committer + "\nmsg\n"},
		{name: "committer without an email", payload: "tree " + testTree + "\ncommitter Octo Cat 1700000000 +0000\n\nmsg\n"},
		{name: "committer with two emails", payload: "tree " + testTree + "\ncommitter Octo <a@example.com> <b@example.com> 1700000000 +0000\n\nmsg\n"},
		{name: "committer without a date", payload: "tree " + testTree + "\ncommitter Octo Cat <octocat@example.com>\n\nmsg\n"},
		{name: "committer with a bad zone", payload: "tree " + testTree + "\ncommitter Octo Cat <octocat@example.com> 1700000000 UTC\n\nmsg\n"},
		{name: "NUL in a header", payload: "tree " + testTree + "\n" + strings.Replace(committer, "Cat", "C\x00t", 1) + "\nmsg\n"},
		{name: "continuation line first", payload: " tree " + testTree + "\n" + committer + "\nmsg\n"},
		{name: "header without a value", payload: "tree\n" + committer + "\nmsg\n"},
		{
			name: "tag",
			payload: "object " + testParent + "\ntype commit\ntag v1.0\n" +
				"tagger Octo Cat <octocat@example.com> 1700000000 +0000\n\nrelease\n",
			object: ObjectTag,
		},
		{
			name: "push certificate",
			payload: "certificate version 0.1\npusher Octo Cat <octocat@example.com> 1700000000 +0000\n" +
				"pushee https://github.com/o/r\nnonce 1700000000-abc\n\n" +
				"0000000000000000000000000000000000000000 " + testParent + " refs/heads/main\n",
			object: ObjectPushCertificate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCommitter([]byte(tc.payload))
			if !errors.Is(err, ErrNotACommit) {
				t.Fatalf("ParseCommitter error = %v, want ErrNotACommit", err)
			}
			var refusal *NotACommitError
			if !errors.As(err, &refusal) {
				t.Fatalf("ParseCommitter error = %T, want *NotACommitError", err)
			}
			if refusal.Object != tc.object {
				t.Fatalf("Object = %q, want %q", refusal.Object, tc.object)
			}
		})
	}
}

func TestSignedCommit(t *testing.T) {
	armored := "-----BEGIN SSH SIGNATURE-----\nAAAA\nBBBB\n-----END SSH SIGNATURE-----\n"
	payload := "tree " + testTree + "\n" +
		"committer Octo Cat <octocat@example.com> 1700000000 +0000\n" +
		"\n" +
		"subject\n\nbody\n"
	want := "tree " + testTree + "\n" +
		"committer Octo Cat <octocat@example.com> 1700000000 +0000\n" +
		"gpgsig -----BEGIN SSH SIGNATURE-----\n" +
		" AAAA\n" +
		" BBBB\n" +
		" -----END SSH SIGNATURE-----\n" +
		"\n" +
		"subject\n\nbody\n"
	if got := string(SignedCommit([]byte(payload), []byte(armored))); got != want {
		t.Fatalf("SignedCommit =\n%s\nwant\n%s", got, want)
	}
}

func TestIdentName(t *testing.T) {
	cases := map[string]string{
		"Acme Inc.":         "Acme Inc.",
		`"Acme"`:            "Acme",
		"Acme;":             "Acme",
		"  Acme  ":          "Acme",
		"Acme <x>":          "Acme x",
		`'Ac,me'`:           "Ac,me",
		`\Acme\`:            "Acme",
		"\tAc\x01me\t":      "Ac\x01me",
		":Acme:":            "Acme",
		"Ä \"Acme\" é,":     "Ä \"Acme\" é",
		"Ac\nme":            "Acme",
		".Acme.":            ".Acme.",
		"Triage Factory":    "Triage Factory",
		`" ;:,<>'\`:         "",
		"\x7fAcme":          "\x7fAcme",
		"Octo\x00Cat":       "Octo\x00Cat",
		"<Acme> Industries": "Acme Industries",
	}
	for in, want := range cases {
		if got := IdentName(in); got != want {
			t.Errorf("IdentName(%q) = %q, want %q", in, got, want)
		}
	}
}
