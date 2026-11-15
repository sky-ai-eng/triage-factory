package delegate

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// memoryChat is a conversation a person works in, with helpers that write its
// transcript and its memory the way a turn does, and ask the memory check.
type memoryChat struct {
	*interactiveFixture
	t *testing.T
}

func newMemoryChat(t *testing.T, suffix string) memoryChat {
	return memoryChat{interactiveFixture: newInteractiveFixture(t, suffix), t: t}
}

func (c memoryChat) insert(m domain.Message) {
	c.t.Helper()
	m.ConversationID = c.conv.ID
	if _, err := c.stores.Conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &m); err != nil {
		c.t.Fatalf("insert %s row %q: %v", m.Role, m.Content, err)
	}
}

// person writes a message, as the person who holds the task.
func (c memoryChat) person(text string) {
	c.t.Helper()
	c.insert(domain.Message{UserID: runmode.LocalDefaultUserID, Role: "user", Content: text})
}

// answer ends a turn with a reply and no tool calls.
func (c memoryChat) answer(text string) {
	c.t.Helper()
	c.insert(domain.Message{Role: "assistant", Content: text})
}

// work ends a turn that ran one tool call before its reply.
func (c memoryChat) work(text string) {
	c.t.Helper()
	c.insert(domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "call-1", Name: "bash",
		Input: map[string]any{"command": "go test ./..."}}}})
	c.insert(domain.Message{Role: "tool", ToolCallID: "call-1", Content: "ok"})
	c.answer(text)
}

// nudged records the note the check asked for, as the driver does.
func (c memoryChat) nudged() {
	c.t.Helper()
	c.insert(domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionNudge, Content: memoryNudgeNote})
}

func (c memoryChat) remember(content string, source domain.MemorySource) {
	c.t.Helper()
	if _, err := c.stores.TaskMemory.UpsertAgentMemorySystem(context.Background(), runmode.LocalDefaultOrgID, c.conv.ID, "", content, source); err != nil {
		c.t.Fatalf("write the conversation's memory: %v", err)
	}
}

// ask is the check, built as an engagement builds it, asked at a turn's end.
func (c memoryChat) ask() string {
	c.t.Helper()
	check := c.s.memoryUpdateNudge(runmode.LocalDefaultOrgID, c.conv.ID, true)
	if check == nil {
		c.t.Fatal("the memory check is not armed for a conversation a person works in")
	}
	return check(context.Background(), 1, "")
}

func (c memoryChat) wantAsked(step string) {
	c.t.Helper()
	if got := c.ask(); got != memoryNudgeNote {
		c.t.Errorf("%s: not asked to update the memory; got %q", step, got)
	}
}

func (c memoryChat) wantQuiet(step string) {
	c.t.Helper()
	if got := c.ask(); got != "" {
		c.t.Errorf("%s: asked to update the memory:\n%s", step, got)
	}
}

// TestMemoryUpdateNudge_ADelegationIsNotAsked: a delegation is checked for a
// memory when it completes, so the turn-end check arms nothing for one.
func TestMemoryUpdateNudge_ADelegationIsNotAsked(t *testing.T) {
	c := newMemoryChat(t, "1450-delegation")
	if check := c.s.memoryUpdateNudge(runmode.LocalDefaultOrgID, c.conv.ID, false); check != nil {
		t.Errorf("the memory check is armed for a delegation: %q", check(context.Background(), 1, ""))
	}
}

// TestMemoryUpdateNudge_AChatThatNeverWroteItsMemory: a chat whose agent never
// wrote the file is asked at the end of its first turn that made tool calls,
// once, and again only after the person writes again. A question answered
// without tool calls is never followed by a note.
func TestMemoryUpdateNudge_AChatThatNeverWroteItsMemory(t *testing.T) {
	c := newMemoryChat(t, "1450-never")

	c.person("what does this ticket want?")
	c.answer("a retry on the flaky upload")
	c.wantQuiet("a question answered without tool calls")

	c.person("try the retry")
	c.work("the retry is in and the tests pass")
	c.wantAsked("a turn that made tool calls, with no memory written")

	c.nudged()
	c.work("done")
	c.wantQuiet("the turn that answered the note")

	c.person("thanks, what was the root cause?")
	c.answer("a timeout shorter than the upload")
	c.wantQuiet("the person wrote again, and the turn made no tool calls")

	c.person("raise the timeout too")
	c.work("raised")
	c.wantAsked("the person wrote again, and the turn made tool calls")
}

// TestMemoryUpdateNudge_AChatWhoseMemoryFellBehind: a chat whose agent wrote
// the file is asked once memoryNudgeAfterPersonMessages of the person's
// messages have arrived without it changing, at the end of the next turn that
// makes tool calls, and not again until the person writes again. Writing the
// same notes again does not count as changing them; writing new ones resets
// the count.
func TestMemoryUpdateNudge_AChatWhoseMemoryFellBehind(t *testing.T) {
	n := memoryNudgeAfterPersonMessages
	c := newMemoryChat(t, "1450-behind")

	c.person("fix the flaky upload")
	c.work("added a retry")
	c.remember("added a retry to the upload", domain.MemorySourceAgent)
	c.wantQuiet("the memory was written this turn")

	for i := 1; i < n; i++ {
		c.person(fmt.Sprintf("follow-up %d", i))
		c.work("done")
		c.wantQuiet(fmt.Sprintf("%d of %d messages since the memory changed", i, n))
	}
	c.person(fmt.Sprintf("follow-up %d", n))
	c.work("done")
	c.wantAsked(fmt.Sprintf("%d messages since the memory changed", n))

	c.nudged()
	c.work("updated")
	// The agent rewrote the notes byte for byte: that is not a change.
	c.remember("added a retry to the upload", domain.MemorySourceAgent)
	c.wantQuiet("asked since the person last wrote")

	c.person("what's left?")
	c.answer("only the PR")
	c.wantQuiet("the person wrote again, but the turn made no tool calls")
	c.person("open the PR")
	c.work("opened")
	c.wantAsked("the next turn that made tool calls, with the notes still unchanged")

	c.nudged()
	c.work("updated")
	c.remember("retry, logging and a cap of five; PR open", domain.MemorySourceAgent)
	c.person("ping the reviewer")
	c.work("pinged")
	c.wantQuiet("one message since the notes changed")
}

// TestMemoryUpdateNudge_AMemoryTheAgentDidNotWriteIsNotItsNotes: a row TF
// generated, or one that records nothing, is not notes the agent wrote, so the
// chat is asked as if it had never written the file.
func TestMemoryUpdateNudge_AMemoryTheAgentDidNotWriteIsNotItsNotes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		source  domain.MemorySource
	}{
		{name: "generated", content: "TF composed this from the transcript", source: domain.MemorySourceGenerated},
		{name: "none", source: domain.MemorySourceNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newMemoryChat(t, "1450-"+tc.name)
			c.person("fix the flaky upload")
			c.work("added a retry")
			c.remember(tc.content, tc.source)
			c.wantAsked("a " + tc.name + " memory row")
		})
	}
}

// TestToolCallsSinceHumanInput pins which turn the memory check reads as the
// one ending: everything after the newest row a person wrote.
func TestToolCallsSinceHumanInput(t *testing.T) {
	human := domain.Message{Role: "user", Content: "fix it"}
	steered := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionSteer, Content: "and the tests"}
	call := domain.Message{Role: "assistant", ToolCalls: []domain.ToolCall{{ID: "c", Name: "bash"}}}
	result := domain.Message{Role: "tool", ToolCallID: "c", Content: "ok"}
	reply := domain.Message{Role: "assistant", Content: "done"}
	nudge := domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionNudge, Content: memoryNudgeNote}

	for _, tc := range []struct {
		name string
		rows []domain.Message
		want bool
	}{
		{name: "an empty transcript"},
		{name: "a reply with no tool calls", rows: []domain.Message{human, reply}},
		{name: "a tool call before the reply", rows: []domain.Message{human, call, result, reply}, want: true},
		{name: "a result with no call row", rows: []domain.Message{human, result, reply}, want: true},
		{name: "tool calls only before the person's newest message", rows: []domain.Message{human, call, result, reply, human, reply}},
		{name: "a steer is the person writing", rows: []domain.Message{human, call, result, steered, reply}},
		{name: "a note TF wrote is not the person writing", rows: []domain.Message{human, call, result, reply, nudge, reply}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolCallsSinceHumanInput(tc.rows); got != tc.want {
				t.Errorf("toolCallsSinceHumanInput = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFirstNudge: the composed hook sends at most one note, the first armed
// check's that has one, and arms nothing when no check is armed.
func TestFirstNudge(t *testing.T) {
	says := func(note string, asked *int) func(context.Context, int, string) string {
		return func(context.Context, int, string) string { *asked++; return note }
	}
	if firstNudge(nil, nil) != nil {
		t.Error("no armed check composed into an armed hook")
	}
	var a, b int
	if got := firstNudge(nil, says("", &a), says("second", &b))(context.Background(), 1, ""); got != "second" || a != 1 || b != 1 {
		t.Errorf("a quiet first check: got %q (asked %d, %d), want the second's note", got, a, b)
	}
	a, b = 0, 0
	if got := firstNudge(says("first", &a), says("second", &b))(context.Background(), 1, ""); got != "first" || b != 0 {
		t.Errorf("two checks with notes: got %q (second asked %d), want the first's alone", got, b)
	}
}

// TestDispatch_AnInteractiveFollowUpIsAskedAboutItsMemory: a person's next
// message reaches an SDK chat through the resume, and that engagement asks
// the memory check as the first one does.
func TestDispatch_AnInteractiveFollowUpIsAskedAboutItsMemory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := newMemoryChat(t, "1450-resume")
	ctx := context.Background()
	const session = "sess-1450-resume"
	c.endTurn(t, session, "the build is red because a test is flaky")
	c.s.dispatchClaimedConversation(ctx, c.claim(t, "why is the build red?"), time.Now())
	if _, err := c.database.Exec(`UPDATE conversations SET model = 'claude-sonnet-4-6' WHERE id = ?`, c.conv.ID); err != nil {
		t.Fatalf("stamp the model: %v", err)
	}

	resumed := c.endTurn(t, session, "fixed")
	if err := c.s.SendMessage(ctx, runmode.LocalDefaultOrgID, c.conv.ID, runmode.LocalDefaultUserID, "fix it", domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	claimed, err := c.stores.ConversationQueue.ClaimNextConversation(ctx, "interactive-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil || claimed == nil || claimed.ID != c.conv.ID {
		t.Fatalf("ClaimNextConversation = (%v, %v), want the chat", claimed, err)
	}
	claimed.OrgID = runmode.LocalDefaultOrgID
	c.s.dispatchClaimedConversation(ctx, claimed, time.Now())

	if resumed.opts.SessionID != session {
		t.Fatalf("the follow-up launched session %q, want %q resumed", resumed.opts.SessionID, session)
	}
	if resumed.nudge == nil {
		t.Fatal("the follow-up's engagement arms no turn-end check")
	}
	c.work("fixed")
	if got := resumed.nudge(ctx); got != memoryNudgeNote {
		t.Errorf("a follow-up that made tool calls with no memory written: got %q, want the memory note", got)
	}
}
