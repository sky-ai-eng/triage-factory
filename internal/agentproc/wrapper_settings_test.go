package agentproc

import (
	"os/exec"
	"testing"
)

// settingsStubSDK stands in for @anthropic-ai/claude-agent-sdk to report, with
// each user message the wrapper pushes, the model and effort the query was on
// when the message arrived: the launch's options until setModel and
// applyFlagSettings move them. setModel refuses the model "unknown".
const settingsStubSDK = `
export function query({ prompt, options }) {
  let model = options.model ?? ""
  let effort = options.effort ?? ""
  return {
    async *[Symbol.asyncIterator]() {
      for await (const msg of prompt) {
        yield { type: "stub_user_message", content: msg.message.content, model, effort }
      }
    },
    async interrupt() {},
    async setPermissionMode() {},
    async setModel(m) {
      if (m === "unknown") throw new Error("no such model")
      model = m
    },
    async applyFlagSettings(s) { effort = s.effortLevel === null ? "<default>" : s.effortLevel },
  }
}
`

// TestWrapperSetSettingsLandsBeforeTheNextMessage: --effort reaches the
// query's options beside --model, and a set_settings control moves both
// before a user_message written after it is pushed, so the turn that message
// starts runs on them. An empty effort clears it to Claude Code's default.
// Each set_settings is answered under its id, and a refused move says so.
func TestWrapperSetSettingsLandsBeforeTheNextMessage(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	w := startWrapper(t, node, settingsStubSDK, "--model", "haiku", "--effort", "low")

	expect := func(content, model, effort string) {
		t.Helper()
		got := w.next()
		if got["type"] != "stub_user_message" || got["content"] != content || got["model"] != model || got["effort"] != effort {
			t.Fatalf("message = %#v, want %q on (%s, %s)", got, content, model, effort)
		}
	}

	w.control(t, map[string]any{"kind": "user_message", "text": "first"})
	expect("first", "haiku", "low")

	settled := func(id string, applied bool) {
		t.Helper()
		got := w.next()
		if got["subtype"] != "settings_result" || got["id"] != id || got["applied"] != applied {
			t.Fatalf("answer = %#v, want settings_result %s applied=%v", got, id, applied)
		}
		if _, hasErr := got["error"]; hasErr == applied {
			t.Errorf("answer = %#v: an error belongs on a refused move only", got)
		}
	}

	w.control(t, map[string]any{"kind": "set_settings", "id": "1", "model": "opus", "effort": "max"})
	w.control(t, map[string]any{"kind": "user_message", "text": "second"})
	settled("1", true)
	expect("second", "opus", "max")

	w.control(t, map[string]any{"kind": "set_settings", "id": "2", "model": "sonnet", "effort": ""})
	w.control(t, map[string]any{"kind": "user_message", "text": "third"})
	settled("2", true)
	expect("third", "sonnet", "<default>")

	w.control(t, map[string]any{"kind": "set_settings", "id": "3", "model": "unknown", "effort": "high"})
	settled("3", false)
}
