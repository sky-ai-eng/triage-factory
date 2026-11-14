package ghwrite

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Addressed reads. A run's touched entities are the pull requests and issues
// it addressed, and a read through the real-`gh` channel addresses one the way
// a write does: by the repository and number the request spells. It can also
// name one without spelling either: by a comment on it, which a REST read names
// by the comment's own id (CommentRef), or by its node id, which a GraphQL
// read names in `node(id:)`. This file names what a read addressed for the two
// transports, and nothing more. It decides nothing and records nothing; the
// injector reads what GitHub calls the object before reporting it, and for a
// comment or a node id first asks GitHub which pull request or issue that is,
// so a request that names an object that does not exist, or one the run's
// credential cannot read, touches nothing.
//
// The rule is the exec verbs': addressed, touch; returned in a set, never. A
// list (`gh pr list`) and a search return sets, and name no object here.
//
// A lookup by branch name (`pullRequests(headRefName:)`) returns a set too,
// but it is how `gh pr view`, `gh pr status` and `gh pr checks` find the pull
// request of the current branch when they are given no number, so it names the
// branch it looks up (BranchRef). The injector resolves the branch with a
// lookup of its own, and touches the one pull request open on it, or nothing
// when none or several are. The rule follows the request, not the command
// that sent it: `gh pr list --head <branch>` sends the same lookup.
//
// An agent can address an object without reading it, by sending a request
// that only spells it, the branch it is open on, a comment on it, or its node
// id. That gains nothing a real read would not: the object is linked only once
// GitHub has answered for it under the run's own credential, which is what
// reading it takes.

// ObjectRef is the pull request or issue one read addressed: its repository as
// the request spelled it, and its number as the request spelled it or, for the
// pull request a branch lookup found and the object a comment is on, as GitHub
// answered it. For the object a node id names, GitHub answered both. GitHub
// numbers the two in one sequence, so the number alone does not say which it
// is.
type ObjectRef struct {
	Owner  string
	Repo   string
	Number int
}

// Key identifies the object among the ones a run has addressed. It folds case:
// GitHub's names are case-insensitive.
func (r ObjectRef) Key() string {
	return strings.ToLower(r.Owner+"/"+r.Repo) + "#" + strconv.Itoa(r.Number)
}

// ReadObservation is one addressed read as the injector saw it: the object the
// request addressed, and what GitHub answered the injector for it, its node id,
// which of the two it is, and its repository's id and current name
// (domain.GitHubIdentity). The run's touched entity is resolved from the
// identity alone; the spelling is a display key.
//
// The sidecar fills this in and the orchestrator records the touch, the split
// Observation keeps for a write.
type ReadObservation struct {
	ObjectRef
	Identity domain.GitHubIdentity
}

// BranchRef is the branch one read looked pull requests up by, as the request
// spelled it: the repository by owner and name, and the head branch by name.
// The request names no head owner, and the injector looks the branch up among
// the repository's own. That is not always the branch the client meant: gh
// sends the name alone for a fork's branch too.
type BranchRef struct {
	Owner  string
	Repo   string
	Branch string
}

// Key identifies the branch among the ones a run has looked up. The repository
// folds case, as ObjectRef.Key does; the branch does not, since git's branch
// names are case-sensitive.
func (r BranchRef) Key() string {
	return strings.ToLower(r.Owner+"/"+r.Repo) + ":" + r.Branch
}

// CommentRef is a comment one REST read addressed by its own id, as the path
// spelled it: its repository, the numbering it is in
// (domain.GitHubCommentSpace), and its number. The comment is on one pull
// request or issue, which the path does not name.
type CommentRef struct {
	Owner string
	Repo  string
	Space domain.GitHubCommentSpace
	ID    int64
}

// AddressedReads is what one GraphQL read addressed: the pull requests and
// issues it selects by number, the branches it looks pull requests up by, and
// the node ids it selects objects by.
//
// Repositories is every repository the read selects at its top level, by
// owner and name, whatever it selects under each, deduplicated and not
// bounded by maxAddressedReads: it is what the injector's repository gate
// checks, and a bound would leave the repositories past it unchecked.
// UnreadRepository reports a top-level repository whose owner or name could
// not be read in the shape of one, which therefore names a repository nobody
// can check.
type AddressedReads struct {
	Objects  []ObjectRef
	Branches []BranchRef
	Nodes    []string

	Repositories     []RepoRef
	UnreadRepository bool
}

// Empty reports whether the read addressed no object, branch or node. The
// repositories it names are not touches, and do not count.
func (a AddressedReads) Empty() bool {
	return len(a.Objects) == 0 && len(a.Branches) == 0 && len(a.Nodes) == 0
}

// maxAddressedReads bounds how many objects, branches and node ids, together,
// one GraphQL request may address. Each costs the injector a read of its own. A
// porcelain query addresses one; the bound keeps a crafted document from
// spending the injector's reads on a list it spelled out.
const maxAddressedReads = 16

// ClassifyRead names the pull request or issue a REST read addresses: a GET of
// the object itself, or of anything under it (its files, its comments, its
// reviews, its timeline), which are attributes of that object. ok is false for
// every other request, including a collection and a comment addressed by its
// own id, whose path names no pull request or issue (ClassifyCommentRead names
// that comment).
func ClassifyRead(method, path string) (ObjectRef, bool) {
	if method != "GET" {
		return ObjectRef{}, false
	}
	owner, repo, rest, ok := splitRepoPath(path)
	if !ok || len(rest) < 2 || (rest[0] != "pulls" && rest[0] != "issues") {
		return ObjectRef{}, false
	}
	n, err := strconv.Atoi(rest[1])
	if err != nil || n <= 0 || !validRepoPart(owner) || !validRepoPart(repo) {
		return ObjectRef{}, false
	}
	return ObjectRef{Owner: owner, Repo: repo, Number: n}, true
}

// commentCollections maps the collection a comment's path enters to the
// numbering its comments are in: /issues/comments/{id} is a conversation
// comment, /pulls/comments/{id} a review comment.
var commentCollections = map[string]domain.GitHubCommentSpace{
	"issues": domain.GitHubIssueComment,
	"pulls":  domain.GitHubReviewComment,
}

// ClassifyCommentRead names the comment a REST read addresses by its own id: a
// GET of /repos/{o}/{r}/issues/comments/{id} or /pulls/comments/{id}, or of
// anything under it (its reactions). ok is false for every other request,
// including a repository's list of comments. A comment is an attribute of the
// pull request or issue it is on, so the read addresses that object; the path
// does not say which it is, and the injector reads the comment to find out.
func ClassifyCommentRead(method, path string) (CommentRef, bool) {
	if method != "GET" {
		return CommentRef{}, false
	}
	owner, repo, rest, ok := splitRepoPath(path)
	if !ok || len(rest) < 3 || rest[1] != "comments" {
		return CommentRef{}, false
	}
	space, ok := commentCollections[rest[0]]
	if !ok {
		return CommentRef{}, false
	}
	id, err := strconv.ParseInt(rest[2], 10, 64)
	if err != nil || id <= 0 || !validRepoPart(owner) || !validRepoPart(repo) {
		return CommentRef{}, false
	}
	return CommentRef{Owner: owner, Repo: repo, Space: space, ID: id}, true
}

// The fields a GraphQL read addresses an object through: the repository at the
// query's top level, by owner and name, and under it the pull request or
// issue, by number, or the pull requests of a branch, by its name. They are the
// finders `gh` sends and the shape a hand-written query takes. Beside the
// repository, a node at the top level addresses an object by its id.
const (
	readRepositoryField = "repository"
	readOwnerArgument   = "owner"
	readNameArgument    = "name"
	readNumberArgument  = "number"
	readBranchField     = "pullRequests"
	readBranchArgument  = "headRefName"
	readNodeField       = "node"
	readIDArgument      = "id"
)

var readObjectFields = map[string]bool{
	"pullRequest":        true,
	"issue":              true,
	"issueOrPullRequest": true,
}

// readTopLevelFields and readRepositoryFields are the fields whose arguments
// the walk reads, at a query's top level and under a repository. Every other
// field's are skipped.
var (
	readTopLevelFields   = map[string]bool{readRepositoryField: true, readNodeField: true}
	readRepositoryFields = func() map[string]bool {
		fields := map[string]bool{readBranchField: true}
		for field := range readObjectFields {
			fields[field] = true
		}
		return fields
	}()
)

// ParseGraphQLRead names the pull requests and issues one GraphQL request
// addresses, the branches it looks pull requests up by, and the node ids it
// selects objects by: each `repository(owner:, name:)` at the top level of the
// query that runs, and each `pullRequest`, `issue` or
// `issueOrPullRequest(number:)`, and each `pullRequests(headRefName:)`,
// selected directly under it, and each `node(id:)` at that top level, with
// aliases resolved. It names each such repository too, whatever is selected
// under it (AddressedReads.Repositories). Empty for a mutation (the write
// audit's), for a query that addresses nothing, and for a document the reader
// cannot walk.
//
// It reads five argument values and no others: the repository's owner and
// name, the object's number, the branch's name, and the node's id, each either
// a variable, resolved from the request's variables, or a literal. A value is
// accepted only in the shape of what it names (validRepoPart, a positive
// integer, validBranchName, validNodeID) and is otherwise dropped with what it
// would have named. Every other argument, every nested selection and every
// fragment is skipped as balanced punctuation, exactly as the mutation reader
// skips them (gqldoc.go).
func ParseGraphQLRead(body []byte) AddressedReads {
	envelope, ok := decodeGraphQLEnvelope(body)
	if !ok || envelope.Query == "" {
		return AddressedReads{}
	}
	// Most reads that address nothing never mention a repository or a node,
	// and stop here.
	if !strings.Contains(envelope.Query, readRepositoryField) && !strings.Contains(envelope.Query, readNodeField) {
		return AddressedReads{}
	}
	ops, walked := parseReadOperations(envelope.Query)
	if !walked {
		return AddressedReads{}
	}
	op, selected := selectReadOperation(ops, envelope.OperationName)
	if !selected || op.typ != gqlOperationQuery || (len(op.reads) == 0 && len(op.repositories) == 0) {
		return AddressedReads{}
	}

	var vars map[string]json.RawMessage
	if len(envelope.Variables) > 0 {
		// Variables that are not an object resolve no variable, which costs
		// only what a variable named.
		_ = json.Unmarshal(envelope.Variables, &vars)
	}
	var out AddressedReads
	repositories := map[string]bool{}
	for _, named := range op.repositories {
		owner, okOwner := named.owner.repoPart(vars)
		repo, okRepo := named.name.repoPart(vars)
		if !okOwner || !okRepo {
			out.UnreadRepository = true
			continue
		}
		ref := RepoRef{Owner: owner, Repo: repo}
		if !repositories[ref.Key()] {
			repositories[ref.Key()] = true
			out.Repositories = append(out.Repositories, ref)
		}
	}
	objects, branches, nodes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, read := range op.reads {
		if read.node != (gqlArgument{}) {
			// A node id is case-sensitive, so it is not folded.
			if id, ok := read.node.nodeID(vars); ok && !nodes[id] {
				nodes[id] = true
				out.Nodes = append(out.Nodes, id)
			}
			continue
		}
		owner, ok := read.owner.repoPart(vars)
		if !ok {
			continue
		}
		repo, ok := read.name.repoPart(vars)
		if !ok {
			continue
		}
		if read.branch != (gqlArgument{}) {
			branch, ok := read.branch.branchName(vars)
			if !ok {
				continue
			}
			ref := BranchRef{Owner: owner, Repo: repo, Branch: branch}
			if !branches[ref.Key()] {
				branches[ref.Key()] = true
				out.Branches = append(out.Branches, ref)
			}
			continue
		}
		number, ok := read.number.objectNumber(vars)
		if !ok {
			continue
		}
		ref := ObjectRef{Owner: owner, Repo: repo, Number: number}
		if !objects[ref.Key()] {
			objects[ref.Key()] = true
			out.Objects = append(out.Objects, ref)
		}
	}
	return out
}

// gqlArgument is one argument's value as the read walk keeps it: the name of
// the variable it is given, or the literal token. The zero value is an
// argument the field did not carry, or carried as something that is neither.
type gqlArgument struct {
	variable string
	literal  gqlToken
}

// stringValue resolves the argument to a string: a string variable, or a
// single-line string literal's characters as written. A literal spelled with an
// escape keeps its backslash, so the shape its caller checks refuses it rather
// than decoding it. A block string keeps no characters, so it resolves to the
// empty string, which no shape admits.
func (a gqlArgument) stringValue(vars map[string]json.RawMessage) (string, bool) {
	switch {
	case a.variable != "":
		var value string
		if err := json.Unmarshal(vars[a.variable], &value); err != nil {
			return "", false
		}
		return value, true
	case a.literal.kind == gqlKindValue && a.literal.quoted:
		return a.literal.text, true
	default:
		return "", false
	}
}

// repoPart resolves the argument to a repository owner or name, in the shape
// validRepoPart admits.
func (a gqlArgument) repoPart(vars map[string]json.RawMessage) (string, bool) {
	value, ok := a.stringValue(vars)
	return value, ok && validRepoPart(value)
}

// nodeID resolves the argument to a node id, in the shape validNodeID admits.
func (a gqlArgument) nodeID(vars map[string]json.RawMessage) (string, bool) {
	value, ok := a.stringValue(vars)
	return value, ok && validNodeID(value)
}

// branchName resolves the argument to a branch name, in the shape
// validBranchName admits.
func (a gqlArgument) branchName(vars map[string]json.RawMessage) (string, bool) {
	value, ok := a.stringValue(vars)
	return value, ok && validBranchName(value)
}

// objectNumber resolves the argument to a pull request or issue number: an
// integer variable or an integer literal, positive.
func (a gqlArgument) objectNumber(vars map[string]json.RawMessage) (int, bool) {
	var text string
	switch {
	case a.variable != "":
		text = strings.TrimSpace(string(vars[a.variable]))
	case a.literal.kind == gqlKindValue && !a.literal.quoted:
		text = a.literal.text
	default:
		return 0, false
	}
	n, err := strconv.Atoi(text)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// maxRepoPartLength is past any owner or repository name GitHub admits (39 and
// 100 characters).
const maxRepoPartLength = 100

// validRepoPart reports whether s has the shape of a repository's owner or
// name: letters, digits, '.', '_' and '-', and not a path's "." or "..". The
// value goes into the path of a request the injector makes with the run's
// credential, so nothing that could address another path is admitted.
func validRepoPart(s string) bool {
	if s == "" || len(s) > maxRepoPartLength || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isNameContinue(c) && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// ValidRepository reports whether owner and repo have the shape of a
// repository's owner and name (validRepoPart), for a repository that came
// from somewhere other than a request this package read: the injector puts it
// in the path of a request it makes with the run's credential.
func ValidRepository(owner, repo string) bool {
	return validRepoPart(owner) && validRepoPart(repo)
}

// maxBranchNameLength bounds the branch names the reader accepts. Git sets no
// length of its own; a longer name costs only its touch.
const maxBranchNameLength = 255

// branchNameRefused are the bytes git refuses anywhere in a ref name, besides
// the control characters.
const branchNameRefused = " ~^:?*[\\"

// validBranchName reports whether s has the shape of a branch name, by the
// rules git applies to one: those `git check-ref-format` applies to
// refs/heads/<s>, which refuse a control character, space, '~', '^', ':', '?',
// '*', '[' or '\' anywhere, "..", "@{" or "//", a leading or trailing '/', a
// trailing '.', and a '/'-separated component starting with '.' or ending in
// ".lock"; and those `git branch` adds, which refuse "HEAD" and a leading '-'.
// It must also be UTF-8, which git does not require but every string GitHub's
// API carries is. The value goes, escaped, into the query of a request the
// injector makes with the run's credential.
func validBranchName(s string) bool {
	if s == "" || len(s) > maxBranchNameLength || s == "HEAD" || s[0] == '-' || !utf8.ValidString(s) {
		return false
	}
	if strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//") ||
		strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f || strings.IndexByte(branchNameRefused, c) >= 0 {
			return false
		}
	}
	for _, component := range strings.Split(s, "/") {
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

// gqlAddressed is one object, branch or node a query addresses, before its
// arguments are resolved against the request's variables. A pull request or
// issue carries its repository and number, a branch lookup its repository and
// branch, and a node its id alone; the rest is zero.
type gqlAddressed struct {
	owner, name    gqlArgument
	number, branch gqlArgument
	node           gqlArgument
}

// gqlReadOperation is one operation definition as the read walk sees it: which
// kind it is, its name, the objects, branches and nodes its top level
// addresses, and the repositories its top level selects. Only a query's are
// collected.
type gqlReadOperation struct {
	typ          string
	name         string
	reads        []gqlAddressed
	repositories []gqlRepository
}

// gqlRepository is one repository a query selects at its top level, before
// its owner and name are resolved against the request's variables. An
// argument given as something that is neither a variable nor a literal is
// kept as the zero gqlArgument, which resolves to nothing.
type gqlRepository struct {
	owner, name gqlArgument
}

// selectReadOperation resolves which operation of a document runs, by the rule
// selectOperation applies to the mutation reader's.
func selectReadOperation(ops []gqlReadOperation, operationName string) (gqlReadOperation, bool) {
	if operationName != "" {
		for _, op := range ops {
			if op.name == operationName {
				return op, true
			}
		}
		return gqlReadOperation{}, false
	}
	if len(ops) == 1 {
		return ops[0], true
	}
	return gqlReadOperation{}, false
}

// parseReadOperations walks a document's definitions for what each query
// addresses. ok is false for anything it cannot walk exactly, which names
// nothing.
func parseReadOperations(doc string) ([]gqlReadOperation, bool) {
	sc := &gqlScanner{src: doc}
	var ops []gqlReadOperation
	for {
		tok, more := sc.next()
		if !more {
			break
		}
		switch {
		case tok.punct("{"):
			// The shorthand form: a bare selection set is an anonymous query.
			reads, repositories, walked := readQuerySelection(sc)
			if !walked {
				return nil, false
			}
			ops = append(ops, gqlReadOperation{typ: gqlOperationQuery, reads: reads, repositories: repositories})
		case tok.name("fragment"):
			if _, _, walked := parseFragmentDefinition(sc); !walked {
				return nil, false
			}
		case tok.name(gqlOperationQuery), tok.name(gqlOperationMutation), tok.name(gqlOperationSubscription):
			op, walked := parseReadOperation(sc, tok.text)
			if !walked {
				return nil, false
			}
			ops = append(ops, op)
		default:
			return nil, false
		}
	}
	if sc.bad {
		return nil, false
	}
	return ops, true
}

// parseReadOperation reads one operation definition with its keyword already
// consumed. A query's selection is walked for what it addresses; any other
// operation's is skipped whole.
func parseReadOperation(sc *gqlScanner, typ string) (gqlReadOperation, bool) {
	op := gqlReadOperation{typ: typ}
	if tok, ok := sc.peek(); ok && tok.kind == gqlKindName {
		sc.next()
		op.name = tok.text
	}
	if tok, ok := sc.peek(); ok && tok.punct("(") {
		sc.next()
		if !sc.skipBalanced("(", ")") {
			return op, false
		}
	}
	if !skipDirectives(sc) {
		return op, false
	}
	tok, ok := sc.next()
	if !ok || !tok.punct("{") {
		return op, false
	}
	if typ != gqlOperationQuery {
		return op, sc.skipBalanced("{", "}")
	}
	var walked bool
	op.reads, op.repositories, walked = readQuerySelection(sc)
	return op, walked
}

// readQuerySelection walks a query's top-level selection set, its opening
// brace already consumed, for each repository it selects by owner and name and
// the objects and branches addressed under it, and each node it selects by id.
// A repository given both arguments is collected whatever their values are, so
// one whose values cannot be read is still one the query names.
func readQuerySelection(sc *gqlScanner) (reads []gqlAddressed, repositories []gqlRepository, walked bool) {
	for {
		field, args, selects, done, ok := readSelectedField(sc, readTopLevelFields, readOwnerArgument, readNameArgument, readIDArgument)
		if !ok {
			return nil, nil, false
		}
		if done {
			return reads, repositories, true
		}
		owner, hasOwner := args[readOwnerArgument]
		name, hasName := args[readNameArgument]
		if field == readRepositoryField && hasOwner && hasName {
			repositories = append(repositories, gqlRepository{owner: owner, name: name})
		}
		if field == readNodeField {
			if id := args[readIDArgument]; id != (gqlArgument{}) && len(reads) < maxAddressedReads {
				reads = append(reads, gqlAddressed{node: id})
			}
			if selects && !sc.skipBalanced("{", "}") {
				return nil, nil, false
			}
			continue
		}
		if !selects {
			continue
		}
		if field != readRepositoryField || owner == (gqlArgument{}) || name == (gqlArgument{}) {
			if !sc.skipBalanced("{", "}") {
				return nil, nil, false
			}
			continue
		}
		members, walked := readRepositorySelection(sc)
		if !walked {
			return nil, nil, false
		}
		for _, read := range members {
			if len(reads) >= maxAddressedReads {
				break
			}
			read.owner, read.name = owner, name
			reads = append(reads, read)
		}
	}
}

// readRepositorySelection walks a repository's selection set, its opening
// brace already consumed, for the number of each pull request or issue it
// selects directly, and the branch of each pull request lookup by branch it
// selects directly. Everything under those fields is skipped.
func readRepositorySelection(sc *gqlScanner) ([]gqlAddressed, bool) {
	var reads []gqlAddressed
	for {
		field, args, selects, done, ok := readSelectedField(sc, readRepositoryFields, readNumberArgument, readBranchArgument)
		if !ok {
			return nil, false
		}
		if done {
			return reads, true
		}
		var read gqlAddressed
		switch {
		case readObjectFields[field]:
			read.number = args[readNumberArgument]
		case field == readBranchField:
			read.branch = args[readBranchArgument]
		}
		if (read.number != (gqlArgument{}) || read.branch != (gqlArgument{})) && len(reads) < maxAddressedReads {
			reads = append(reads, read)
		}
		if selects && !sc.skipBalanced("{", "}") {
			return nil, false
		}
	}
}

// readSelectedField consumes one member of a selection set: a field with its
// alias, arguments and directives, or a fragment, which is skipped whole and
// reported as a field with no name. done reports the set's closing brace.
// selects reports that the field opens a selection set, whose opening brace is
// consumed and whose contents are the caller's to walk or skip.
//
// Arguments are read only for the fields the caller names, and only the ones
// named in want; every other field's are skipped as balanced punctuation.
func readSelectedField(sc *gqlScanner, fields map[string]bool, want ...string) (field string, args map[string]gqlArgument, selects, done, ok bool) {
	tok, more := sc.next()
	if !more {
		return "", nil, false, false, false
	}
	if tok.punct("}") {
		return "", nil, false, true, true
	}
	if tok.punct("...") {
		return "", nil, false, false, skipFragmentSelection(sc)
	}
	if tok.kind != gqlKindName {
		return "", nil, false, false, false
	}
	field = tok.text
	if next, more := sc.peek(); more && next.punct(":") {
		sc.next()
		real, more := sc.next()
		if !more || real.kind != gqlKindName {
			return "", nil, false, false, false
		}
		field = real.text
	}
	if next, more := sc.peek(); more && next.punct("(") {
		sc.next()
		if fields[field] {
			if args, ok = readArguments(sc, want); !ok {
				return "", nil, false, false, false
			}
		} else if !sc.skipBalanced("(", ")") {
			return "", nil, false, false, false
		}
	}
	if !skipDirectives(sc) {
		return "", nil, false, false, false
	}
	if next, more := sc.peek(); more && next.punct("{") {
		sc.next()
		selects = true
	}
	return field, args, selects, false, true
}

// skipFragmentSelection consumes what follows a `...` in a selection set: a
// named spread, or an inline fragment with its selection. Neither is read: a
// repository or an object selected through a fragment names nothing here.
func skipFragmentSelection(sc *gqlScanner) bool {
	if tok, ok := sc.peek(); ok && tok.name("on") {
		sc.next()
		if cond, more := sc.next(); !more || cond.kind != gqlKindName {
			return false
		}
	} else if ok && tok.kind == gqlKindName {
		sc.next()
		return skipDirectives(sc)
	}
	if !skipDirectives(sc) {
		return false
	}
	if tok, ok := sc.peek(); ok && tok.punct("{") {
		sc.next()
		return sc.skipBalanced("{", "}")
	}
	return true
}

// readArguments reads an argument list, its opening parenthesis already
// consumed, keeping the ones named in want whose value is a variable or a
// scalar literal. A list or an object value is skipped as balanced
// punctuation, and an enum, a boolean or a null is passed over.
func readArguments(sc *gqlScanner, want []string) (map[string]gqlArgument, bool) {
	args := map[string]gqlArgument{}
	for {
		tok, more := sc.next()
		if !more {
			return nil, false
		}
		if tok.punct(")") {
			return args, true
		}
		if tok.kind != gqlKindName {
			return nil, false
		}
		if colon, more := sc.next(); !more || !colon.punct(":") {
			return nil, false
		}
		value, more := sc.next()
		if !more {
			return nil, false
		}
		var arg gqlArgument
		switch {
		case value.punct("$"):
			name, more := sc.next()
			if !more || name.kind != gqlKindName {
				return nil, false
			}
			arg.variable = name.text
		case value.punct("["):
			if !sc.skipBalanced("[", "]") {
				return nil, false
			}
		case value.punct("{"):
			if !sc.skipBalanced("{", "}") {
				return nil, false
			}
		case value.kind == gqlKindValue:
			arg.literal = value
		case value.kind == gqlKindName:
		default:
			return nil, false
		}
		for _, name := range want {
			if tok.text == name {
				args[name] = arg
			}
		}
	}
}
