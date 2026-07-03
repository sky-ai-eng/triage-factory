//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// debugMock, when set by the claude profile's -debug-stream flag,
// logs one line per mock request so termination-condition bugs are
// visible instead of manifesting as an infinite conversation.
var debugMock bool

// mockLLM is a per-run stand-in for the Anthropic Messages API, bound
// on the sandbox's gateway IP exactly where production's LLM proxy
// lives. It drives the real agent engine through a scripted multi-turn
// tool loop — each response asks for one Bash tool call until the
// conversation carries `turns` tool results, then ends the turn — so
// the engine's heap grows like a real run's, with zero API spend and
// no credentials anywhere.
type mockLLM struct {
	turns    int
	requests atomic.Int64
}

// start binds on hostIP and serves until the returned closer runs.
func (m *mockLLM) start(hostIP string) (baseURL string, closer func(), err error) {
	ln, err := net.Listen("tcp", hostIP+":0")
	if err != nil {
		return "", nil, fmt.Errorf("mock llm listen on %s: %w", hostIP, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", m.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":128}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"mock: unhandled path"}}`))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// handleMessages scripts the conversation off the request body alone:
// fewer than `turns` tool_result blocks → reply with a Bash tool_use;
// otherwise end the turn. Stateless per request so parallel side
// requests can't wedge the main loop.
func (m *mockLLM) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	m.requests.Add(1)
	results := bytes.Count(body, []byte(`"tool_result"`))
	done := results >= m.turns
	if debugMock {
		maxTok := "?"
		if i := bytes.Index(body, []byte(`"max_tokens":`)); i >= 0 {
			end := i + 13
			for end < len(body) && body[end] >= '0' && body[end] <= '9' {
				end++
			}
			maxTok = string(body[i+13 : end])
		}
		fmt.Printf("  [mock] req=%d bytes=%d tool_results=%d max_tokens=%s stream=%v done=%v\n",
			m.requests.Load(), len(body), results, maxTok,
			bytes.Contains(body, []byte(`"stream":true`)), done)
	}
	if !bytes.Contains(body, []byte(`"stream":true`)) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg_mock","type":"message","role":"assistant","model":"mock","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":256,"output_tokens":64}}`, filler(256))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	sse := func(event, data string) {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if fl != nil {
			fl.Flush()
		}
	}

	// IDs must be unique per response: message and tool_use ids key the
	// client's history bookkeeping, and reusing one across turns keeps
	// the transcript from accumulating — the turn counter then never
	// advances and the conversation loops forever.
	seq := m.requests.Load()
	sse("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_mock_%d","type":"message","role":"assistant","model":"mock","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":256,"output_tokens":1}}}`, seq))
	sse("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	sse("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, filler(1024)))
	sse("content_block_stop", `{"type":"content_block_stop","index":0}`)

	stop := "end_turn"
	if !done {
		stop = "tool_use"
		sse("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_mock_%d","name":"Bash","input":{}}}`, seq))
		sse("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"seq 1 2000 && ls -la /work\"}"}}`)
		sse("content_block_stop", `{"type":"content_block_stop","index":1}`)
	}
	sse("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":300}}`, stop))
	sse("message_stop", `{"type":"message_stop"}`)
}

// filler returns n bytes of plain prose-ish text so transcripts and
// heaps grow like a real conversation's.
func filler(n int) string {
	const chunk = "the quick brown fox jumps over the lazy dog and keeps going because the benchmark needs realistic transcript growth "
	return strings.Repeat(chunk, n/len(chunk)+1)[:n]
}
