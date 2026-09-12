package agentproc

import (
	"encoding/json"
	"os/exec"
	"testing"
)

// echoUserMessagesStubSDK stands in for @anthropic-ai/claude-agent-sdk to read
// back the SDKUserMessages the wrapper pushed onto the input iterable: it
// yields each one's content as the SDK would have received it, so a test can
// assert on the shape rather than on the wrapper's internals.
const echoUserMessagesStubSDK = `
export function query({ prompt }) {
  return {
    async *[Symbol.asyncIterator]() {
      for await (const msg of prompt) {
        yield { type: "stub_user_message", content: msg.message.content }
      }
    },
    async interrupt() {},
    async setPermissionMode() {},
  }
}
`

// TestWrapperUserMessageContentShapes pins both spellings of a user_message
// control against the SHIPPED wrapper: text alone becomes a plain string
// content, and blocks become the block array verbatim. The array is what
// carries an opening turn assembled from several rows — the API's MessageParam
// takes either, and a wrapper that stringified the blocks would hand the model
// one undifferentiated wall of text.
func TestWrapperUserMessageContentShapes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, echoUserMessagesStubSDK)

	w.control(t, map[string]any{"kind": "user_message", "text": "a plain turn"})
	if got := w.next(); got["type"] != "stub_user_message" || got["content"] != "a plain turn" {
		t.Fatalf("text-only content = %#v, want the string %q", got["content"], "a plain turn")
	}

	w.control(t, map[string]any{"kind": "user_message", "blocks": []map[string]string{
		{"type": "text", "text": "<untrusted-input>"},
		{"type": "text", "text": "the task context"},
	}})
	got := w.next()
	blocks, ok := got["content"].([]any)
	if !ok {
		t.Fatalf("blocks content = %#v, want an array of content blocks", got["content"])
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks content = %#v, want both blocks", blocks)
	}
	for i, want := range []string{"<untrusted-input>", "the task context"} {
		block, _ := blocks[i].(map[string]any)
		if block["type"] != "text" || block["text"] != want {
			t.Errorf("block %d = %#v, want a text block %q", i, blocks[i], want)
		}
	}
}

// TestWrapperUserMessageBlocksWinOverText: a control carrying both spellings
// sends the blocks. Nothing in TF writes one today, and the precedence is what
// keeps it that way — a wrapper preferring text would silently send a caller's
// fallback string instead of the turn it assembled.
func TestWrapperUserMessageBlocksWinOverText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, echoUserMessagesStubSDK)

	w.control(t, map[string]any{
		"kind":   "user_message",
		"text":   "the joined fallback",
		"blocks": []map[string]string{{"type": "text", "text": "the real turn"}},
	})
	got := w.next()
	blocks, ok := got["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("content = %#v, want the single block", got["content"])
	}
	if block, _ := blocks[0].(map[string]any); block["text"] != "the real turn" {
		t.Errorf("content = %#v, want the block, not the text field", blocks[0])
	}
}

// control writes one control line to the wrapper's stdin.
func (w *wrapperProc) control(t *testing.T, ctl map[string]any) {
	t.Helper()
	line, err := json.Marshal(ctl)
	if err != nil {
		t.Fatalf("marshal control: %v", err)
	}
	if _, err := w.stdin.Write(append(line, '\n')); err != nil {
		t.Fatalf("write control: %v", err)
	}
}
