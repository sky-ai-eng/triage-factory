package gh

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/todo-triage/internal/ai"
	"github.com/sky-ai-eng/todo-triage/internal/db"
	"github.com/sky-ai-eng/todo-triage/internal/domain"
	ghclient "github.com/sky-ai-eng/todo-triage/internal/github"
)

const defaultOwner = "sky-ai-eng"
const defaultRepo = "sky"

func handlePR(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: todotriage exec gh pr <action> [flags]")
	}

	action := args[0]
	flags := args[1:]

	switch action {
	case "view":
		prView(client, flags)
	case "diff":
		prDiff(client, flags)
	case "files":
		prFiles(client, flags)
	case "thread-view":
		prThreadView(client, flags)
	case "review-view":
		prReviewView(client, flags)
	case "review-delete":
		prReviewDelete(database, flags)
	case "review-dismiss":
		prReviewDismiss(client, flags)
	case "start-review":
		prStartReview(client, database, flags)
	case "add-review-comment":
		prAddReviewComment(database, flags)
	case "submit-review":
		prSubmitReview(client, database, flags)
	case "comment-list-pending":
		prListPending(database, flags)
	case "add-comment":
		prAddComment(client, flags)
	case "comment-reply":
		prCommentReply(client, flags)
	case "comment-react":
		prCommentReact(client, flags)
	case "comment-update":
		prCommentUpdate(client, database, flags)
	case "comment-delete":
		prCommentDelete(client, database, flags)
	default:
		exitErr(fmt.Sprintf("unknown pr action: %s", action))
	}
}

func prView(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	verbose := hasFlag(args, "-v") || hasFlag(args, "--verbose")
	pr, err := client.GetPR(owner, repo, number, verbose)
	exitOnErr(err)
	printJSON(pr)
}

func prDiff(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	file := flagVal(args, "--file")
	diff, err := client.GetPRDiff(owner, repo, number, file)
	exitOnErr(err)
	fmt.Print(diff)
}

func prFiles(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	files, err := client.GetPRFiles(owner, repo, number)
	exitOnErr(err)
	printJSON(files)
}

func prThreadView(client *ghclient.Client, args []string) {
	if len(args) < 2 {
		exitErr("usage: gh pr thread-view <pr_number> <comment_id> [--page N]")
	}
	owner, repo, _ := parseRepoAndNumber(args[:1])
	commentID := mustInt(args[1], "comment_id")
	page := 1
	if v := flagVal(args, "--page"); v != "" {
		page = mustInt(v, "page")
	}
	thread, err := client.GetCommentThread(owner, repo, commentID, page)
	exitOnErr(err)
	printJSON(thread)
}

func prReviewView(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-view <review_id> --pr <number>")
	}
	reviewID := mustInt(args[0], "review_id")
	owner, repo := ownerRepo(args)
	prNumber := mustInt(flagVal(args, "--pr"), "pr_number")
	verbose := hasFlag(args, "-v") || hasFlag(args, "--verbose")
	detail, err := client.GetReviewDetail(owner, repo, prNumber, reviewID, verbose)
	exitOnErr(err)
	printJSON(detail)
}

func prReviewDelete(database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-delete <review_id>")
	}
	reviewID := args[0]
	err := db.DeletePendingReview(database.Conn, reviewID)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "review_id": reviewID})
}

func prReviewDismiss(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr review-dismiss <review_id> --pr <number> --body <reason>")
	}
	reviewID := mustInt(args[0], "review_id")
	owner, repo := ownerRepo(args)
	prNumber := mustInt(flagVal(args, "--pr"), "pr_number")
	body := flagVal(args, "--body")
	if body == "" {
		body = "Dismissed"
	}
	err := client.DismissReview(owner, repo, prNumber, reviewID, body)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true, "review_id": reviewID, "status": "dismissed"})
}

// --- Review lifecycle (local state) ---

func prStartReview(client *ghclient.Client, database *db.DB, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	// Fetch head SHA for the review
	pr, err := client.GetPR(owner, repo, number, false)
	exitOnErr(err)

	// Fetch and parse diff to know which lines are valid comment targets
	diff, err := client.GetPRDiff(owner, repo, number, "")
	exitOnErr(err)

	diffLinesMap := ghclient.DiffLines(diff)
	// Convert to JSON: {"file": [1,2,3,...], ...}
	compactMap := make(map[string][]int)
	for file, lines := range diffLinesMap {
		for line := range lines {
			compactMap[file] = append(compactMap[file], line)
		}
	}
	diffLinesJSON, _ := json.Marshal(compactMap)

	reviewID := uuid.New().String()
	err = db.CreatePendingReview(database.Conn, domain.PendingReview{
		ID:        reviewID,
		PRNumber:  number,
		Owner:     owner,
		Repo:      repo,
		CommitSHA: pr.HeadSHA,
		DiffLines: string(diffLinesJSON),
		RunID:     os.Getenv("TODOTRIAGE_RUN_ID"),
	})
	exitOnErr(err)

	printJSON(map[string]any{
		"review_id":  reviewID,
		"pr_number":  number,
		"commit_sha": pr.HeadSHA,
		"status":     "pending_local",
		"files":      len(diffLinesMap),
	})
}

func prAddReviewComment(database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr add-review-comment <review_id> --file <path> --line <N> --body <text> [--start-line <N>]")
	}
	reviewID := args[0]
	file := flagVal(args, "--file")
	line := mustInt(flagVal(args, "--line"), "line")
	body := flagVal(args, "--body")

	if file == "" || body == "" {
		exitErr("--file and --body are required")
	}

	// Verify review exists
	review, err := db.GetPendingReview(database.Conn, reviewID)
	exitOnErr(err)
	if review == nil {
		exitErr(fmt.Sprintf("pending review %s not found", reviewID))
	}

	// Validate line against diff
	if review.DiffLines != "" {
		var validLines map[string][]int
		if json.Unmarshal([]byte(review.DiffLines), &validLines) == nil {
			fileLines, fileExists := validLines[file]
			if !fileExists {
				exitErr(fmt.Sprintf("file '%s' is not in the diff. Valid files: %v", file, keys(validLines)))
			}
			lineSet := make(map[int]bool, len(fileLines))
			for _, l := range fileLines {
				lineSet[l] = true
			}
			if !lineSet[line] {
				exitErr(fmt.Sprintf("line %d in '%s' is not part of the diff. Comment on lines that appear in the diff output.", line, file))
			}
		}
	}

	commentID := uuid.New().String()
	comment := domain.PendingReviewComment{
		ID:       commentID,
		ReviewID: reviewID,
		Path:     file,
		Line:     line,
		Body:     body,
	}

	if sl := flagVal(args, "--start-line"); sl != "" {
		v := mustInt(sl, "start-line")
		comment.StartLine = &v
	}

	err = db.AddPendingReviewComment(database.Conn, comment)
	exitOnErr(err)

	printJSON(map[string]any{
		"comment_id": commentID,
		"review_id":  reviewID,
		"status":     "pending_local",
	})
}

func prSubmitReview(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr submit-review <review_id> --event <approve|comment|request_changes> --body <text>")
	}
	reviewID := args[0]
	event := flagVal(args, "--event")
	body := flagVal(args, "--body")

	if event == "" {
		exitErr("--event is required (approve, comment, request_changes)")
	}

	eventMap := map[string]string{
		"approve":         "APPROVE",
		"comment":         "COMMENT",
		"request_changes": "REQUEST_CHANGES",
	}
	ghEvent, ok := eventMap[event]
	if !ok {
		ghEvent = event
	}

	// Load pending review
	review, err := db.GetPendingReview(database.Conn, reviewID)
	exitOnErr(err)
	if review == nil {
		exitErr(fmt.Sprintf("pending review %s not found", reviewID))
	}

	// Load pending comments
	pendingComments, err := db.ListPendingReviewComments(database.Conn, reviewID)
	exitOnErr(err)

	// Convert to GitHub format
	ghComments := make([]ghclient.SubmitReviewComment, len(pendingComments))
	for i, c := range pendingComments {
		ghComments[i] = ghclient.SubmitReviewComment{
			Path:      c.Path,
			Line:      c.Line,
			StartLine: c.StartLine,
			Body:      c.Body,
		}
	}

	// In preview mode, store the raw body — the server injects header/footer
	// with actual cost data at submit time.
	if os.Getenv("TODOTRIAGE_REVIEW_PREVIEW") == "1" {
		err = db.SetPendingReviewSubmission(database.Conn, reviewID, body, ghEvent)
		exitOnErr(err)

		printJSON(map[string]any{
			"status":          "pending_approval",
			"review_id":       reviewID,
			"event":           ghEvent,
			"comments_queued": len(ghComments),
		})
		return
	}

	// Inject header and footer with run metadata
	body = buildReviewBody(body, database)

	// Submit atomically to GitHub
	ghReviewID, actualEvent, err := client.SubmitReview(
		review.Owner, review.Repo, review.PRNumber,
		review.CommitSHA, ghEvent, body, ghComments,
	)
	exitOnErr(err)

	// Clean up local state
	if err := db.DeletePendingReview(database.Conn, reviewID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to clean up local review state: %v\n", err)
	}

	printJSON(map[string]any{
		"github_review_id": ghReviewID,
		"event":            actualEvent,
		"comments_posted":  len(ghComments),
	})
}

func prListPending(database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-list-pending <review_id>")
	}
	reviewID := args[0]
	comments, err := db.ListPendingReviewComments(database.Conn, reviewID)
	exitOnErr(err)
	if comments == nil {
		comments = []domain.PendingReviewComment{}
	}
	printJSON(comments)
}

// --- Direct comments (hit GitHub API) ---

func prAddComment(client *ghclient.Client, args []string) {
	owner, repo, number := parseRepoAndNumber(args)
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	commentID, err := client.AddComment(owner, repo, number, body)
	exitOnErr(err)
	printJSON(map[string]any{"comment_id": commentID})
}

func prCommentReply(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-reply <comment_id> --body <text> --pr <number>")
	}
	commentID := mustInt(args[0], "comment_id")
	owner, repo := ownerRepo(args)
	prNumber := mustInt(flagVal(args, "--pr"), "pr_number")
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}
	replyID, err := client.ReplyToComment(owner, repo, prNumber, commentID, body)
	exitOnErr(err)
	printJSON(map[string]any{"reply_id": replyID})
}

func prCommentReact(client *ghclient.Client, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-react <comment_id> --emoji <emoji>")
	}
	commentID := mustInt(args[0], "comment_id")
	owner, repo := ownerRepo(args)
	emoji := flagVal(args, "--emoji")
	if emoji == "" {
		exitErr("--emoji is required (+1, -1, laugh, confused, heart, hooray, rocket, eyes)")
	}
	err := client.ReactToComment(owner, repo, commentID, emoji)
	exitOnErr(err)
	printJSON(map[string]any{"ok": true})
}

func prCommentUpdate(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-update <comment_id> --body <text>")
	}
	commentID := args[0]
	body := flagVal(args, "--body")
	if body == "" {
		exitErr("--body is required")
	}

	// Check if it's a local pending comment (UUID) vs remote (integer)
	if isLocalID(commentID) {
		err := db.UpdatePendingReviewComment(database.Conn, commentID, body)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "local"})
	} else {
		owner, repo := ownerRepo(args)
		id := mustInt(commentID, "comment_id")
		err := client.UpdateComment(owner, repo, id, body)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "remote"})
	}
}

func prCommentDelete(client *ghclient.Client, database *db.DB, args []string) {
	if len(args) < 1 {
		exitErr("usage: gh pr comment-delete <comment_id>")
	}
	commentID := args[0]

	if isLocalID(commentID) {
		err := db.DeletePendingReviewComment(database.Conn, commentID)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "local"})
	} else {
		owner, repo := ownerRepo(args)
		id := mustInt(commentID, "comment_id")
		err := client.DeleteComment(owner, repo, id)
		exitOnErr(err)
		printJSON(map[string]any{"ok": true, "scope": "remote"})
	}
}

// isLocalID returns true if the ID looks like a UUID (local pending comment)
// vs an integer (GitHub remote comment).
func keys[K comparable, V any](m map[K]V) []K {
	result := make([]K, 0, len(m))
	for k := range m {
		result = append(result, k)
	}
	return result
}

func isLocalID(id string) bool {
	if len(id) == 0 {
		return false
	}
	// GitHub IDs are purely numeric, our local IDs are UUIDs (contain hyphens).
	_, err := strconv.Atoi(id)
	return err != nil
}

// --- argument parsing helpers ---

func parseRepoAndNumber(args []string) (string, string, int) {
	owner, repo := ownerRepo(args)
	// Find first positional arg (not a flag or flag value)
	num := firstPositional(args)
	if num == "" {
		exitErr("PR number is required")
	}
	number := mustInt(num, "pr_number")
	return owner, repo, number
}

// firstPositional returns the first argument that isn't a flag or a flag's value.
func firstPositional(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--repo" || a == "--file" || a == "--pr" || a == "--body" || a == "--line" || a == "--start-line" || a == "--event" || a == "--status" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

func ownerRepo(args []string) (string, string) {
	if v := flagVal(args, "--repo"); v != "" {
		parts := splitOwnerRepo(v)
		return parts[0], parts[1]
	}
	return defaultOwner, defaultRepo
}

func splitOwnerRepo(s string) [2]string {
	for i, c := range s {
		if c == '/' {
			return [2]string{s[:i], s[i+1:]}
		}
	}
	return [2]string{s, ""}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func flagVal(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func mustInt(s, name string) int {
	if s == "" {
		exitErr(name + " is required")
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		exitErr(fmt.Sprintf("invalid %s: %s", name, s))
	}
	return v
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func exitOnErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

// buildReviewBody wraps the agent's review body with a metadata footer.
func buildReviewBody(body string, database *db.DB) string {
	runID := os.Getenv("TODOTRIAGE_RUN_ID")
	if runID == "" {
		return body + "\n\n---\n*This review was partially generated by AI.*"
	}

	// Look up run for duration info
	run, err := db.GetAgentRun(database.Conn, runID)
	if err != nil || run == nil {
		return body + "\n\n---\n*This review was partially generated by AI.*"
	}

	// Sum tokens and calculate cost
	totals, err := db.RunTokenTotals(database.Conn, runID)
	if err != nil {
		return body + "\n\n---\n*This review was partially generated by AI.*"
	}

	model := totals.Model
	if model == "" {
		model = run.Model
	}

	cost := ai.CalculateCostUSD(model, totals.InputTokens, totals.OutputTokens, totals.CacheReadTokens, totals.CacheCreationTokens)

	// Pretty-print model name
	modelDisplay := model
	switch {
	case strings.Contains(model, "opus"):
		modelDisplay = "Claude Opus"
	case strings.Contains(model, "sonnet"):
		modelDisplay = "Claude Sonnet"
	case strings.Contains(model, "haiku"):
		modelDisplay = "Claude Haiku"
	}

	// Calculate elapsed time from run start
	elapsed := prettyElapsed(time.Since(run.StartedAt))

	footer := fmt.Sprintf("\n\n---\n*This review was partially generated by AI.* Time: %s | Model: %s | Cost: ~$%.3f", elapsed, modelDisplay, cost)

	return body + footer
}

func prettyElapsed(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	s = s % 60
	if m < 60 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := m / 60
	m = m % 60
	return fmt.Sprintf("%dh %dm", h, m)
}
