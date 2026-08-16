package hook

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/pushpolicy"
)

// ExitRefused is the exit status `hook check-push` uses to say "this push is
// refused by policy". The pre-push hook aborts the push on THIS status alone
// and lets every other status through, which is what makes the guard fail
// open by construction: a missing binary (127), a panic (2), a DB that won't
// open — none of them can block a push.
const ExitRefused = 3

// runCheckPush is the local-mode enforcement point for the team's base-branch
// push policy. The pre-push hook feeds it the refs git is about to push (on
// stdin, in git's own pre-push format) and aborts the push when this returns
// ExitRefused.
//
// It is a SAFETY GUARD, not a security control. The agent runs as the operator
// with unrestricted bash, so `git push --no-verify` — or a rewritten remote —
// walks straight past it, and it is not meant to stop an agent that tries.
// It is meant to stop the agent that never thought about it and ran the naive
// command, which is the failure that actually happens.
//
// Every outcome other than a positive "this ref is protected and the policy
// says no" allows the push: no run context, an unreadable database, a remote
// that isn't the org's GitHub host, an unparseable line. A mistake-guard that
// fails closed on a dead database is a broken tool, not a safer one.
func runCheckPush(host agenthost.Client, stores db.Stores, args []string, refsIn io.Reader) int {
	fs := flag.NewFlagSet("hook check-push", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	remote := fs.String("remote", "", "remote URL being pushed to (git's $2)")
	if err := fs.Parse(args); err != nil {
		return 0 // malformed invocation: allow (flag already printed the error)
	}
	refs := readPushRefs(refsIn)
	if *remote == "" || len(refs) == 0 {
		return 0
	}

	// Same bound as record-push: the hook must never hold git open on a wedged
	// read, and on timeout we allow.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Same remote gate as record-push: a push to something that isn't the org's
	// GitHub host isn't ours to judge.
	owner, repo, ok := parseRemoteOwnerRepo(*remote, orgGitHubHost(ctx, host))
	if !ok {
		return 0
	}
	repoRef := domain.RepoRef{Owner: owner, Repo: repo}

	info, err := host.LookupRun(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook check-push: no run context (%v); allowing push\n", err)
		return 0
	}
	protected, err := pushpolicy.ProtectedFor(ctx, stores, info.OrgID, info.TeamID, repoRef, info.IsEventTriggered)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hook check-push: could not evaluate base-branch push policy (%v); allowing push\n", err)
		return 0
	}

	for _, ref := range refs {
		branch, isBranch := strings.CutPrefix(ref, "refs/heads/")
		if !isBranch || !protected[branch] {
			continue
		}
		// Wording tracks the git proxy's ref-level denial (internal/gitproxy
		// refDenial) so the same mistake reads the same way in either mode.
		fmt.Fprintf(os.Stderr,
			"triagefactory: refusing to push %s — %s is a protected branch on %s (its base/default branch) and this team's base-branch push policy refuses pushes to it. Commit to a new branch and open a pull request instead. A team admin can change the policy in Settings → Team.\n",
			ref, branch, repoRef.Slug())
		return ExitRefused
	}
	return 0
}

// readPushRefs parses git's pre-push stdin format — one
// "<local ref> SP <local sha> SP <remote ref> SP <remote sha>" line per ref —
// and returns the REMOTE refs, which are what the policy is about (the local
// name can differ under a push refspec). Deletes are included: dropping the
// base branch is the mistake this guard most wants to catch.
//
// The shape is checked strictly — exactly four fields, with a remote ref that
// actually looks like one — and anything else is skipped. Loose parsing would
// undo the fail-open contract from the inside: a line this function had to
// guess at could put an arbitrary token in the ref position, and a wrong guess
// here doesn't degrade to "allow", it refuses a push that was never asking to
// touch a protected branch. git emits exactly this format, so a line that
// doesn't match is not evidence of anything and is treated as such.
func readPushRefs(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 4 || !strings.HasPrefix(fields[2], "refs/") {
			continue
		}
		out = append(out, fields[2])
	}
	return out
}
