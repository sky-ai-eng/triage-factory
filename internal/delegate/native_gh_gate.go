// The native runtime's pre-dispatch matcher on three `gh` commands: merging a
// pull request, posting a review, and creating a repository. All three are
// refused, and all three are refused again — for real — at the credential
// injector every request on this channel passes through.
//
// That is the whole of what this file is now: early UX in front of an enforced
// policy. It exists because the same answer arrives better from a matcher than
// from a proxy. A refusal here costs one tool call and names the redirect in
// the model's own terms; the injector's arrives as a 403 partway through a `gh`
// invocation, after the model has already committed to a plan built around the
// command succeeding.
//
// So the limitations that used to be caveats are now merely uninteresting. This
// matcher is injection-blind and trivially evadable — `gh api graphql` with an
// inline mutation walks straight past it, as does any client that is not `bash`
// — and none of that matters, because nothing here is load-bearing. What the
// three refusals must be is TRUE: a model that discovers a stated rule is false
// has cause to doubt every other rule it was given, and a matcher that said
// "this is refused" while the act quietly succeeded would be exactly that. The
// enforcement is in internal/ghinjector, keyed on the shared classifier in
// internal/ghwrite; these texts say the same thing that gate says, and change
// when it changes.

package delegate

import (
	"context"
	"path"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewRefusal answers `gh pr review` every time. It is deliberately plain
// text rather than a marked note: the model should read it as an ordinary
// tool error, and re-run the call it should have made. Repetition is not
// badgering when the answer never changes and the redirect is actionable.
//
// The verbs it names are the ones the native harness prompt documents; the
// two must stay in step, since a redirect to a command that does not exist is
// worse than no redirect at all.
//
// The justification is verb dominance, and only that. It used to also say such
// a review was invisible to Triage Factory, which stopped being true when the
// channel's writes started landing audit rows — the review verbs win because a
// review posted raw cannot anchor to a line, carry a severity badge, be revised
// as a draft, or go through the approval path, not because nobody would know.
const reviewRefusal = "This command was not run, and the same call made another way will be refused too. " +
	"`gh pr review` cannot attach comments to specific lines, and it posts straight to GitHub — " +
	"no draft to revise, no severity badges, no approval path. " +
	"Use `tfac gh pr start-review`, then `add-review-comment` for each finding, then `finalize-review`."

// mergeRefusal answers a merge attempt every time.
//
// It used to be a question — "does your mission tell you to merge? quote the
// line" — which was the right shape while the runtime could not know the
// answer and nothing downstream enforced one. It is the wrong shape now: the
// injector refuses the merge whatever the mission says, so a model that found
// its authorization, quoted it, and re-issued would be told no by a 403. Asking
// a question whose only correct answer is refused teaches the model that this
// harness's rules do not mean what they say.
const mergeRefusal = "This command was not run, and the same call made another way will be refused too. " +
	"Triage Factory does not merge with a run's credential — not on any mission, and not with any " +
	"authorization, because merging is irreversible and no human is watching this run. " +
	"Finish your work and push the branch, then say in your final message that you believe it is " +
	"ready to merge and let a human do it."

// repoCreateRefusal answers `gh repo create` every time.
//
// It names no replacement command, unlike the review refusal, because there is
// no `tfac exec gh repo` verb to name — and the file comment above holds that a
// redirect to a command that does not exist is worse than no redirect at all.
// So it redirects to the only thing that actually works: saying so, and letting
// a human do it.
//
// TODO(TFAC-793): when a governed repo-provisioning verb exists, name it here.
// That ticket also carries the prior question of whether an agent should be able
// to ask for a repository at all, in which case this text stands permanently.
const repoCreateRefusal = "This command was not run, and the same call made another way will be refused too. " +
	"Creating repositories is not something a run does — Triage Factory scopes polling, task routing, " +
	"and this run's own credential to a set of repositories chosen before the run started, and a new one " +
	"belongs to none of it. " +
	"If your mission genuinely needs a repository that does not exist, say so in your final message and stop there."

// ghCommandGate builds the native loop's BeforeToolCall hook.
//
// A refusal is a synthetic is_error result the model reads in-band, so it never
// ends a run — it redirects, and the redirect is a call the model can make
// instead. Nothing but the three matched shapes is looked at, so a run that
// attempts none of them sees no evidence this exists.
func (s *Spawner) ghCommandGate() func(context.Context, domain.ToolCall) string {
	return func(_ context.Context, call domain.ToolCall) string {
		switch classifyGHCommand(bashCommand(call)) {
		case ghActionReview:
			return reviewRefusal
		case ghActionRepoCreate:
			return repoCreateRefusal
		case ghActionMerge:
			return mergeRefusal
		}
		return ""
	}
}

// bashCommand extracts the shell command a call would run, or "" for any call
// that is not a shell at all.
func bashCommand(call domain.ToolCall) string {
	if call.Name != "bash" {
		return ""
	}
	cmd, _ := call.Input["command"].(string)
	return cmd
}

// ghAction is the gated action a shell command was recognized as reaching for.
type ghAction int

const (
	ghActionNone ghAction = iota
	ghActionMerge
	ghActionReview
	ghActionRepoCreate
)

// classifyGHCommand reports which gated action, if any, a shell command
// reaches for.
//
// The shape matched is a `gh` invocation whose subcommand chain is `pr merge`
// or `pr review`, plus a `gh api` call naming a `/merge` endpoint. Leading
// environment assignments, an absolute path and flags interleaved with the
// subcommand chain are tolerated because they are what a model writes; the
// scan crosses shell separators for the same reason, since `cd /work && gh pr
// merge 7` is one bash call. It is not exhaustive and is not trying to be —
// see the file comment.
func classifyGHCommand(command string) ghAction {
	for _, words := range shellSegments(command) {
		if a := classifyGHSegment(words); a != ghActionNone {
			return a
		}
	}
	return ghActionNone
}

// classifyGHSegment classifies one simple command.
func classifyGHSegment(words []string) ghAction {
	i := 0
	for i < len(words) && isEnvAssignment(words[i]) {
		i++
	}
	// `env FOO=bar gh …` reaches the same place by a different spelling.
	if i < len(words) && words[i] == "env" {
		i++
		for i < len(words) && isEnvAssignment(words[i]) {
			i++
		}
	}
	if i >= len(words) || path.Base(words[i]) != "gh" {
		return ghActionNone
	}
	args := words[i+1:]
	chain := subcommandChain(args)
	switch {
	case len(chain) == 0:
		return ghActionNone
	case chain[0] == "api":
		if namesMergeEndpoint(args) {
			return ghActionMerge
		}
		if namesRepoCreate(args) {
			return ghActionRepoCreate
		}
	case chain[0] == "pr" && len(chain) > 1:
		switch chain[1] {
		case "merge":
			return ghActionMerge
		case "review":
			return ghActionReview
		}
	case chain[0] == "repo" && len(chain) > 1:
		if chain[1] == "create" {
			return ghActionRepoCreate
		}
	}
	return ghActionNone
}

// subcommandChain returns the first two positional words of a gh invocation —
// its subcommand chain.
//
// A word following a bare flag is skipped as that flag's value. Which flags
// take one is not knowable here, and reading the value of `--repo owner/repo`
// as a subcommand would miss the shape it appears in; skipping one word too
// many can only cost a match, never invent one.
func subcommandChain(words []string) []string {
	var chain []string
	for j := 0; j < len(words) && len(chain) < 2; j++ {
		w := words[j]
		if strings.HasPrefix(w, "-") && w != "-" {
			if !strings.Contains(w, "=") {
				j++
			}
			continue
		}
		chain = append(chain, w)
	}
	return chain
}

// namesMergeEndpoint reports whether any argument names a REST path whose
// last resource is `merge`.
//
// A whole path segment, not a substring: `/merges` merges two branches and
// `/merge-upstream` syncs a fork, neither of which is the irreversible thing
// this gate is about, and a field value that happens to contain the word is
// not an endpoint at all.
func namesMergeEndpoint(words []string) bool {
	for _, w := range words {
		p, _, _ := strings.Cut(w, "?")
		p, _, _ = strings.Cut(p, "#")
		for i, seg := range strings.Split(p, "/") {
			// i > 0 keeps this to a path: the segment must be reached
			// through a `/`, so a bare word `merge` is not an endpoint.
			if i > 0 && seg == "merge" {
				return true
			}
		}
	}
	return false
}

// repoCreateMutations are the two ways GitHub's schema makes a repository. Both
// are what `gh repo create` itself sends, so a hand-written call is reaching for
// the same act by a spelling the subcommand match does not see.
var repoCreateMutations = []string{"createRepository", "cloneTemplateRepository"}

// namesRepoCreate reports whether a `gh api` call reaches for repository
// creation — the REST collections that mint one, or a GraphQL document naming a
// mutation that does.
//
// Unlike the merge endpoint, both shapes here are addressed by paths that are
// ALSO ordinary reads: `/user/repos` lists your repositories, and a schema query
// may mention the mutation by name. Refusing those would make the control cost
// more than it saves, so this half reads the method too — which for `gh api`
// means the explicit flag when there is one and otherwise whether any field was
// passed, since that is when gh switches from GET to POST on its own.
//
// Still substring-shaped and still evadable, which is what an accident control
// is; see the file comment.
func namesRepoCreate(words []string) bool {
	if !apiPosts(words) {
		return false
	}
	for _, w := range words {
		if namesRepoCreateMutation(w) || mintsRepoByPath(w) {
			return true
		}
	}
	return false
}

// namesRepoCreateMutation reports whether a word is the GraphQL `query` field
// carrying a document that makes a repository.
//
// Scoped to that one field for the reason namesMergeEndpoint scopes itself to
// path segments: a field value that happens to contain the word is not the act.
// `gh api …/comments -f body='…createRepository…'` is a comment about repository
// creation, and refusing it would be refusing ordinary work — a live risk in
// this repository in particular, where a pull request discussing this very gate
// contains those identifiers as prose.
//
// A document passed by file (`-F query=@doc.graphql`, `--input doc.json`) is
// invisible here and always was. It no longer needs chasing: the injector reads
// the request body itself, so a document this scan cannot see is refused where
// it actually arrives.
func namesRepoCreateMutation(word string) bool {
	for _, prefix := range []string{"--field=", "--raw-field="} {
		word = strings.TrimPrefix(word, prefix)
	}
	document, isQuery := strings.CutPrefix(word, "query=")
	if !isQuery {
		return false
	}
	for _, name := range repoCreateMutations {
		if strings.Contains(document, name) {
			return true
		}
	}
	return false
}

// mintsRepoByPath reports whether a word is a REST path that creates a
// repository. Three do: the two collections a repository can be posted to, and
// the template-instantiation endpoint, which addresses the TEMPLATE and makes a
// new repository out of it.
func mintsRepoByPath(word string) bool {
	p, _, _ := strings.Cut(word, "?")
	p, _, _ = strings.Cut(p, "#")
	segs := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	// A repo-scoped path also ends in a segment pair, so the leading collection
	// is what tells /user/repos apart from repos/{owner}/{repo}.
	case len(segs) == 2 && segs[0] == "user" && segs[1] == "repos":
		return true
	case len(segs) == 3 && segs[0] == "orgs" && segs[2] == "repos":
		return true
	case len(segs) == 4 && segs[0] == "repos" && segs[3] == "generate":
		return true
	}
	return false
}

// apiPosts reports whether a `gh api` invocation issues a POST: the method flag
// says so, or no method flag is present and something was passed that makes gh
// switch off GET on its own — a field, or a body file.
//
// `--input` counts for the same reason a field does. gh documents its own
// ruleset-creation example as `gh api …/rulesets --input file.json` with no
// method flag, which only works because supplying a body implies the POST.
//
// An explicit non-POST method loses, even alongside a body — `-X GET -f q=x`
// sends a GET with a query string, and refusing that would be refusing a read.
func apiPosts(words []string) bool {
	body := false
	for i, w := range words {
		switch {
		case w == "-X" || w == "--method":
			return i+1 < len(words) && strings.EqualFold(words[i+1], "POST")
		case strings.HasPrefix(w, "--method="):
			return strings.EqualFold(strings.TrimPrefix(w, "--method="), "POST")
		case strings.HasPrefix(w, "-X") && len(w) > 2:
			return strings.EqualFold(w[2:], "POST")
		case w == "-f", w == "-F", w == "--field", w == "--raw-field", w == "--input",
			strings.HasPrefix(w, "--field="), strings.HasPrefix(w, "--raw-field="),
			strings.HasPrefix(w, "--input="):
			body = true
		}
	}
	return body
}

// isEnvAssignment reports whether a word is a leading `NAME=VALUE` assignment
// rather than the command itself.
func isEnvAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// shellSegments splits a command line into simple commands, each already
// word-split with quotes stripped.
//
// It is a reader, not a shell: quoting decides where words end, unquoted
// separators decide where commands end, and nothing is expanded. Text inside
// quotes therefore stays one word, which is what keeps `--body "gh pr merge is
// refused"` from reading as an invocation — and, symmetrically, is why
// `bash -c "gh pr merge 7"` is not seen as one.
func shellSegments(command string) [][]string {
	var (
		segs  [][]string
		words []string
		cur   []rune
		open  bool // a word is being built — possibly an empty one, from ""
		quote rune
	)
	endWord := func() {
		if open {
			words = append(words, string(cur))
			cur = cur[:0]
			open = false
		}
	}
	endSegment := func() {
		endWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}

	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if quote != 0 {
			switch {
			case c == quote:
				quote = 0
			case quote == '"' && c == '\\' && i+1 < len(rs):
				i++
				cur = append(cur, rs[i])
			default:
				cur = append(cur, c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			open = true
		case '\\':
			if i+1 < len(rs) {
				i++
				// A line continuation joins what follows onto this command;
				// any other escaped rune is a literal character in the word.
				if rs[i] != '\n' {
					cur = append(cur, rs[i])
					open = true
				}
			}
		case ' ', '\t', '\r':
			endWord()
		case ';', '&', '|', '\n', '(', ')', '{', '}', '`':
			endSegment()
		default:
			cur = append(cur, c)
			open = true
		}
	}
	endSegment()
	return segs
}
