// The native runtime's pre-dispatch gate on three `gh` commands.
//
// The jail ships the real `gh` on a PATH-leading directory with a working
// credential channel, so a delegated agent can merge a pull request or post a
// review outside Triage Factory entirely. Both are intercepted here, and they
// get different answers because the situations are not alike. A review posted
// through `gh` can only be a top-level comment, lands no artifact and takes no
// approval path, so the TF review verbs strictly dominate it: that is a refusal
// with a redirect. (The injector does now audit such a review — it is a write
// under the org credential like any other — but an audit row is a record of
// what happened, not a substitute for the review object the product works on.)
// Merging is something some missions are for, so refusing it would mean
// deciding an intent the runtime cannot know: that gets a question, aimed at
// the one artifact the model can actually check. Creating a repository is
// refused outright, and for a reason that is neither of those: the product is
// built around knowing which repositories an agent works in BEFORE a run
// starts — the tracked set is what scopes polling, task routing, and the
// credential itself — so a repository that appears mid-run is outside every
// structure meant to account for it. No mission needs one badly enough to
// invent it unattended.
//
// These are accident controls, not security controls, and three limitations
// come with them, accepted rather than worked around:
//
//   - Injection-blind. The same hostile context that induced the action will
//     satisfy a self-attestation just as easily.
//   - Matcher-evadable. Any construction this matcher does not recognize —
//     `gh api graphql` with an inline mutation being the obvious one — passes
//     straight through.
//   - Self-attestation only. Nothing checks the answer to the merge question,
//     and a model that simply re-issues the call proceeds.
//
// What they do work against is the failure they target: a model reaching for
// the command it knows best, or building momentum toward a helpful-seeming
// finish. The value of the merge question is the forced restatement of intent
// against the mission, not the obstruction — a model asked to quote its
// authorization and unable to find one frequently abandons the action. The
// actual boundary is the credential's repo scope, which none of this changes.

package delegate

import (
	"context"
	"path"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
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
const reviewRefusal = "This command was not run. `gh pr review` cannot attach comments to specific lines, " +
	"and it posts straight to GitHub — no artifact, no approval path. " +
	"Use `tfac gh pr start-review`, then `add-review-comment` for each finding, then `finalize-review`."

// repoCreateRefusal answers `gh repo create` every time.
//
// It names no replacement command, unlike the review refusal, because there is
// no `tfac exec gh repo` verb to name — and the file comment above holds that a
// redirect to a command that does not exist is worse than no redirect at all.
// So it redirects to the only thing that actually works: saying so, and letting
// a human do it. When a verb exists, this text is where it gets named.
const repoCreateRefusal = "This command was not run. Creating repositories is not something a run does — " +
	"Triage Factory scopes polling, task routing, and this run's own credential to a set of repositories " +
	"chosen before the run started, and a new one belongs to none of it. " +
	"If your mission genuinely needs a repository that does not exist, say so in your final message and stop there."

// mergeGateTag opens the merge question and is how a later call recognizes
// that it has already been put. The identity lives in the tag, not in the
// prose, so the wording can be changed without silently re-arming the question
// on every conversation mid-flight.
const mergeGateTag = `<system-note kind="merge-gate">`

// mergeGateQuestion is what a merge attempt is asked before it runs.
//
// It points at the opening message rather than at "what the user asked for":
// there is no user present, and a model's sense of what was asked is a vibe,
// while the opening turn is a specific artifact in its context that
// demonstrably does or does not carry the instruction — and demonstrably does
// not carry one that arrived in a PR comment.
const mergeGateQuestion = mergeGateTag + "\n" +
	"This command was not run. Merging is not reversible and no human is watching this run. " +
	"Before you do it: does your mission — the opening message of this conversation — actually tell you to merge? " +
	"If it does, quote that line before merging. If you cannot find one, do not merge — finish your work and say " +
	"in your final message that you believe the branch is ready to merge, and let a human do it.\n" +
	"</system-note>"

// ghCommandGate builds the native loop's BeforeToolCall hook.
//
// A denial is a synthetic is_error result the model reads in-band, so neither
// answer ever ends a run: the refusal redirects and the question is answerable
// by re-issuing. Nothing but the two matched shapes is looked at, so a run
// that attempts neither sees no evidence the gate exists — and only a matched
// merge pays for the transcript read.
func (s *Spawner) ghCommandGate(orgID, runID string) func(context.Context, domain.ToolCall) string {
	return func(ctx context.Context, call domain.ToolCall) string {
		switch classifyGHCommand(bashCommand(call)) {
		case ghActionReview:
			return reviewRefusal
		case ghActionRepoCreate:
			return repoCreateRefusal
		case ghActionMerge:
			if s.mergeAlreadyQuestioned(ctx, orgID, runID) {
				return ""
			}
			return mergeGateQuestion
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

// mergeAlreadyQuestioned reads the question's state off the transcript.
//
// A read failure asks again, which is the cheap way to be wrong: the model
// answers a question it has already answered and re-issues, costing one turn.
// Staying quiet would instead let the one call this exists for through on the
// strength of a failed read.
func (s *Spawner) mergeAlreadyQuestioned(ctx context.Context, orgID, runID string) bool {
	rows, err := s.agentRuns.ListForAssemblySystem(ctx, orgID, runID)
	if err != nil {
		delegateLog.Warn("read transcript for the merge question failed; asking again", "run", runID, "error", err)
		return false
	}
	return askedAboutMergeAlready(rows)
}

// askedAboutMergeAlready reports whether the merge question has been put with
// no human input since.
//
// The state is the transcript, never process state, so a crash and a re-claim
// behave identically to the engagement that asked. Walking back from the
// newest row: the gate's own note means the question stands and the model has
// already restated its intent against it, so asking again would be
// obstruction rather than reconsideration. Genuine human input newer than the
// note means a person spoke since — the premise changed, and the same question
// about that new work has not been put. Everything else the system wrote on
// the agent's behalf speaks for no one and is skipped, using the same closed
// human set the engine's drain and turn budget key on.
//
// The note check comes first, because the row it looks for is system-authored
// and the human filter below would skip it.
func askedAboutMergeAlready(rows []domain.Message) bool {
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		if isMergeGateNote(r) {
			return true
		}
		if r.Role == "user" && agentloop.IsHumanInput(r) {
			return false
		}
	}
	return false
}

// isMergeGateNote reports whether a row is one this gate itself wrote.
//
// The tag has to open an is_error tool result, not merely appear somewhere in
// a row: it is a string that exists in this repository and in every transcript
// the note lands in, so a run that greps its own source, cats a log, or reads
// a conversation would otherwise hand itself a row that spends the question it
// was never asked. Anchoring it leaves everything after the tag free to
// change, which is the whole reason the identity is a tag and not the prose.
func isMergeGateNote(r domain.Message) bool {
	return r.Role == "tool" && r.IsError && strings.HasPrefix(r.Content, mergeGateTag)
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
		for _, name := range repoCreateMutations {
			if strings.Contains(w, name) {
				return true
			}
		}
		p, _, _ := strings.Cut(w, "?")
		p, _, _ = strings.Cut(p, "#")
		segs := strings.Split(strings.Trim(p, "/"), "/")
		// /user/repos and /orgs/{org}/repos are the two collections that mint a
		// repository. A repo-scoped path also ends in a segment pair, so the
		// leading collection is what tells them apart.
		if len(segs) == 2 && segs[0] == "user" && segs[1] == "repos" {
			return true
		}
		if len(segs) == 3 && segs[0] == "orgs" && segs[2] == "repos" {
			return true
		}
	}
	return false
}

// apiPosts reports whether a `gh api` invocation issues a POST: the method flag
// says so, or no method flag is present and a field was passed, which is when
// gh posts of its own accord.
//
// An explicit non-POST method loses, even alongside fields — `-X GET -f q=x`
// sends a GET with a query string, and refusing that would be refusing a read.
func apiPosts(words []string) bool {
	fields := false
	for i, w := range words {
		switch {
		case w == "-X" || w == "--method":
			return i+1 < len(words) && strings.EqualFold(words[i+1], "POST")
		case strings.HasPrefix(w, "--method="):
			return strings.EqualFold(strings.TrimPrefix(w, "--method="), "POST")
		case strings.HasPrefix(w, "-X") && len(w) > 2:
			return strings.EqualFold(w[2:], "POST")
		case w == "-f", w == "-F", w == "--field", w == "--raw-field",
			strings.HasPrefix(w, "--field="), strings.HasPrefix(w, "--raw-field="):
			fields = true
		}
	}
	return fields
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
