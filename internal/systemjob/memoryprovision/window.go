package memoryprovision

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// WindowTokenBudget is how much transcript one generation sends, in tokens
// estimated at windowBytesPerToken. It is deliberately half of the smallest
// window TF will run this job on: the runtime adds its own system material on
// both paths, and the local path's estimator refuses client-side at about 10%
// above the real count, so a budget that merely fit would refuse on the
// transcripts it matters most for.
const WindowTokenBudget = 100_000

// windowBytesPerToken is the estimator: bytes divided by three. Conservative
// for English prose and roughly right for the code and JSON a transcript is
// mostly made of, and cheap — the alternative is tokenizing a megabyte of
// text to decide what to send.
const windowBytesPerToken = 3

// windowByteBudget is the budget the walk actually spends, since every unit
// it measures is a byte.
const windowByteBudget = WindowTokenBudget * windowBytesPerToken

// elisionMarkerReserve is held back from the row budget so the marker line the
// walk may add afterwards is paid for rather than charged to nobody — without
// it a head and tail that exactly fill the budget render a transcript over it.
// Reserved unconditionally, even when nothing is elided: the walk cannot know
// whether it needs the line until it has already spent the budget deciding, and
// thirty-odd bytes out of three hundred thousand is not worth a second pass to
// reclaim. Sized for a count wider than any transcript this will ever meet.
const elisionMarkerReserve = len("[… 18446744073709551615 rows elided …]\n")

// toolContentLimit caps a rendered tool result. A single 100 KB file read
// must not spend the whole tail: the memory needs the shape of what the agent
// did, and the first few KB of a result carry that where the rest does not.
const toolContentLimit = 4 << 10

// rowContentLimit caps every other row's rendered content. It is the same
// argument one size up, and it is what makes "never split a row" safe to
// promise: no single row can exceed a fraction of the budget, so the walk can
// never spend everything on one message and send a window with nothing in it.
const rowContentLimit = 64 << 10

// toolCallArgsLimit caps the rendered arguments of one tool call. The name
// and the target are what a memory needs from a call ("Edit on
// internal/foo.go"); the whole argument object is the file it wrote.
const toolCallArgsLimit = 200

// truncationMarker says a render was cut, so a reader never mistakes a clipped
// value for the whole of what was there.
const truncationMarker = "[… truncated …]"

// roleAssistant / roleUser are messages.role values. The column is
// app-validated rather than CHECKed and the vocabulary lives in
// ConversationStore's doc comment; these two are the ones the windowing reads.
const (
	roleAssistant = "assistant"
	roleUser      = "user"
	roleTool      = "tool"
)

// openingTurnSubtype is the subtype an opening turn composed by TF carries.
// The head rule takes the first ordinary user row OR the first opening turn,
// whichever comes first — a native transcript opens with one of these, an SDK
// transcript writes no opening row and opens with the human's own message.
const openingTurnSubtype = "injection:task-context"

// window is one generation's view of a transcript: the rendered text, and the
// two counts that say how much of the conversation it represents. The counts
// are recorded on the attempt row, so a thin memory reads as a truncated
// window rather than as a model with nothing to say.
type window struct {
	text      string
	rowsTotal int
	rowsSent  int
}

// buildWindow renders rows into a bounded transcript: head first, then tail.
//
// The head is the conversation's opening — everything up to and including the
// first ordinary user row (the opening turn a native run mints, or the first
// human message on an SDK transcript) — and it is what makes the tail legible,
// since it names the task and the state the agent inherited. The tail is rows
// from the newest backwards, and it is where the state a memory needs lives:
// the branch, the last commits, what was mid-flight. The middle is the part a
// memory can most afford to lose, so it is what goes.
//
// Both runtimes are served by the one walk. A native transcript may already
// carry a compaction result, which is a row like any other and rides along in
// the tail; an SDK transcript is the raw mirror with no summaries in it at
// all, so the tail rule is the only thing bounding it.
func buildWindow(rows []domain.Message) window {
	return buildWindowWithin(rows, windowByteBudget)
}

// buildWindowWithin is buildWindow against an explicit byte budget, so the
// walk's boundaries — the exact fill in particular — can be exercised at sizes
// a test can write out rather than only at the production budget.
func buildWindowWithin(rows []domain.Message, byteBudget int) window {
	total := len(rows)
	if total == 0 {
		return window{}
	}
	// What the rows themselves may spend; the remainder is the marker's.
	rowBudget := byteBudget - elisionMarkerReserve

	rendered := make([]string, total)
	for i, r := range rows {
		rendered[i] = renderRow(r)
	}

	// The head boundary: one past the first ordinary user row. A transcript
	// with no such row — every row an injection, say — has no opening to
	// anchor on and is all tail, which is the right answer rather than a
	// reason to take the first N rows of something.
	headEnd := 0
	for i, r := range rows {
		if r.Role == roleUser && (r.Subtype == "" || r.Subtype == openingTurnSubtype) {
			headEnd = i + 1
			break
		}
	}

	// Each row costs its render plus the newline that joins it to the next.
	spent, head := 0, 0
	for i := 0; i < headEnd; i++ {
		cost := len(rendered[i]) + 1
		if spent+cost > rowBudget {
			break
		}
		spent += cost
		head = i + 1
	}

	tailStart := total
	for i := total - 1; i >= head; i-- {
		cost := len(rendered[i]) + 1
		if spent+cost > rowBudget {
			break
		}
		spent += cost
		tailStart = i
	}

	var b strings.Builder
	b.Grow(spent)
	for i := 0; i < head; i++ {
		b.WriteString(rendered[i])
		b.WriteByte('\n')
	}
	if elided := tailStart - head; elided > 0 {
		fmt.Fprintf(&b, "[… %d rows elided …]\n", elided)
	}
	for i := tailStart; i < total; i++ {
		b.WriteString(rendered[i])
		b.WriteByte('\n')
	}

	return window{
		text:      b.String(),
		rowsTotal: total,
		rowsSent:  head + (total - tailStart),
	}
}

// renderRow renders one message as the transcript lines it contributes:
// `role[/subtype]: content`, followed by one line per tool call the row
// carries. Nothing here is the message's storage shape — tokens, costs,
// reasoning and content blocks are all dropped, because a memory is about
// what the agent did rather than about what the turn cost.
func renderRow(m domain.Message) string {
	head := m.Role
	if m.Subtype != "" {
		head += "/" + m.Subtype
	}

	limit := rowContentLimit
	if m.Role == roleTool {
		limit = toolContentLimit
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString(": ")
	b.WriteString(truncateBytes(m.Content, limit))
	for _, tc := range m.ToolCalls {
		b.WriteString("\ntool_call ")
		b.WriteString(tc.Name)
		b.WriteString("(")
		b.WriteString(renderToolCallInput(tc.Input))
		b.WriteString(")")
	}
	return b.String()
}

// renderToolCallInput renders a tool call's arguments, bounded. Unmarshalable
// input renders as a placeholder rather than failing the row: the call's name
// is most of what a memory needs from it, and a render is not the place to
// discover that a stored blob is malformed.
func renderToolCallInput(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	b, err := json.Marshal(input)
	if err != nil {
		return truncationMarker
	}
	return truncateBytes(string(b), toolCallArgsLimit)
}

// truncateBytes caps s at limit bytes and says so when it cuts. The cut backs
// up to a rune boundary so it never garbles the last character — a transcript
// carries whatever the agent read, which is frequently not ASCII.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncationMarker
}

// utf8RuneStart reports whether b begins a rune: anything but a continuation
// byte (0b10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
