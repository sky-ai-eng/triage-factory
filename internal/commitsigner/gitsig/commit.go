package gitsig

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Identity is a committer's name and email as the commit's committer header
// spells them.
type Identity struct {
	Name  string
	Email string
}

// IdentName returns the name git writes into an ident line, such as a commit's
// committer header, when it is given name as user.name. Git trims bytes up to
// and including space, and the characters , : ; < > " \ ', from both ends, and
// drops newlines, < and > anywhere else (strbuf_addstr_without_crud in git's
// ident.c). A name compared against what ParseCommitter returns has to be
// passed through this first, or a profile name like "Acme" (quotes included)
// never matches the Acme git wrote. Git applies the same rule to user.email.
func IdentName(name string) string {
	start, end := 0, len(name)
	for start < end && identCrud(name[start]) {
		start++
	}
	for end > start && identCrud(name[end-1]) {
		end--
	}
	var b strings.Builder
	b.Grow(end - start)
	for i := start; i < end; i++ {
		switch c := name[i]; c {
		case '\n', '<', '>':
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// identCrud is git's crud(): what IdentName trims from either end of a name.
// It compares bytes, so a multi-byte UTF-8 character is never crud.
func identCrud(c byte) bool {
	switch c {
	case ',', ':', ';', '<', '>', '"', '\\', '\'':
		return true
	}
	return c <= ' '
}

// What NotACommitError.Object names when a payload is an object git signs
// that is not a commit.
const (
	// ObjectTag is an annotated tag, from git tag -s.
	ObjectTag = "tag"
	// ObjectPushCertificate is a push certificate, from git push --signed.
	ObjectPushCertificate = "push certificate"
)

// ErrNotACommit is wrapped by every refusal ParseCommitter returns.
var ErrNotACommit = errors.New("gitsig: not a commit")

// NotACommitError is ParseCommitter's refusal. Object is ObjectTag or
// ObjectPushCertificate when the payload is one of those, and empty when it
// is no object git signs. Reason says what is wrong with it.
type NotACommitError struct {
	Object string
	Reason string
}

func (e *NotACommitError) Error() string {
	return ErrNotACommit.Error() + ": " + e.Reason
}

func (e *NotACommitError) Unwrap() error { return ErrNotACommit }

// ParseCommitter reads payload as the commit object git asks its signing
// program to sign and returns the committer it names. It reads only the header
// block, the lines before the first blank line. It refuses a payload with no
// blank line, a tag or push certificate, and a commit without exactly one tree
// header and one committer header. The committer's timestamp is checked for
// form only.
func ParseCommitter(payload []byte) (Identity, error) {
	end := bytes.Index(payload, []byte("\n\n"))
	if end < 0 {
		return Identity{}, notACommit("", "no blank line ends the header block")
	}
	headers, err := parseHeaders(string(payload[:end]))
	if err != nil {
		return Identity{}, err
	}
	switch headers[0].key {
	case "object":
		return Identity{}, notACommit(ObjectTag, "the payload is a tag object")
	case "certificate":
		return Identity{}, notACommit(ObjectPushCertificate, "the payload is a push certificate")
	case "tree":
	default:
		return Identity{}, notACommit("", fmt.Sprintf("the first header is %q, not tree", headers[0].key))
	}

	var trees, committers []string
	for _, h := range headers {
		switch h.key {
		case "tree":
			trees = append(trees, h.value)
		case "committer":
			committers = append(committers, h.value)
		}
	}
	if len(trees) != 1 {
		return Identity{}, notACommit("", fmt.Sprintf("%d tree headers, want 1", len(trees)))
	}
	// CommitID hashes with SHA-1, so a tree named by any other kind of id is
	// a repository whose commit ids this package cannot compute.
	if !isSHA1Hex(trees[0]) {
		return Identity{}, notACommit("", fmt.Sprintf("tree %q is not a SHA-1 object id", trees[0]))
	}
	if len(committers) != 1 {
		return Identity{}, notACommit("", fmt.Sprintf("%d committer headers, want 1", len(committers)))
	}
	id, ok := parseIdent(committers[0])
	if !ok {
		return Identity{}, notACommit("", fmt.Sprintf("committer %q is not name <email> seconds zone", committers[0]))
	}
	return id, nil
}

type header struct {
	key, value string
}

// parseHeaders splits a header block, without its final newline, into
// headers. A line that starts with a space continues the header before it, as
// a mergetag's do; its content is not kept.
func parseHeaders(block string) ([]header, error) {
	if strings.IndexByte(block, 0) >= 0 {
		return nil, notACommit("", "the header block holds a NUL byte")
	}
	var headers []header
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, " ") {
			if len(headers) == 0 {
				return nil, notACommit("", "the header block starts with a continuation line")
			}
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok || key == "" {
			return nil, notACommit("", fmt.Sprintf("header line %q is not key value", line))
		}
		headers = append(headers, header{key: key, value: value})
	}
	return headers, nil
}

// parseIdent splits an ident in the one form git writes: "name <email>
// seconds zone", where git has already removed any < or > from the name and
// the email.
func parseIdent(s string) (Identity, bool) {
	name, rest, ok := strings.Cut(s, " <")
	if !ok || strings.ContainsAny(name, "<>") {
		return Identity{}, false
	}
	email, date, ok := strings.Cut(rest, "> ")
	if !ok || strings.ContainsAny(email, "<>") {
		return Identity{}, false
	}
	seconds, zone, ok := strings.Cut(date, " ")
	if !ok || len(zone) != 5 || (zone[0] != '+' && zone[0] != '-') || !isDigits(zone[1:]) {
		return Identity{}, false
	}
	if _, err := strconv.ParseUint(seconds, 10, 64); err != nil {
		return Identity{}, false
	}
	return Identity{Name: name, Email: email}, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isSHA1Hex(s string) bool {
	if len(s) != hex.EncodedLen(sha1.Size) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

func notACommit(object, reason string) error {
	return &NotACommitError{Object: object, Reason: reason}
}

// SignedCommit returns the commit object git writes once its signing program
// returns armored for payload. armored goes in as a gpgsig header at the end
// of payload's header block, each of its lines prefixed by one space and the
// first by the header name as well.
func SignedCommit(payload, armored []byte) []byte {
	at := len(payload)
	if i := bytes.Index(payload, []byte("\n\n")); i >= 0 {
		at = i + 1
	}
	var sig bytes.Buffer
	for rest := armored; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
		}
		if sig.Len() == 0 {
			sig.WriteString("gpgsig")
		}
		sig.WriteByte(' ')
		sig.Write(line)
		rest = rest[len(line):]
	}
	out := make([]byte, 0, len(payload)+sig.Len())
	out = append(out, payload[:at]...)
	out = append(out, sig.Bytes()...)
	return append(out, payload[at:]...)
}

// CommitID returns the id git gives a commit object: the hex SHA-1 of
// "commit <size>\0" followed by the object.
func CommitID(object []byte) string {
	framed := append([]byte("commit "+strconv.Itoa(len(object))+"\x00"), object...)
	sum := sha1.Sum(framed)
	return hex.EncodeToString(sum[:])
}
