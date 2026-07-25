package inference

import (
	"reflect"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func f64(v float64) *float64 { return &v }
func boolPtr(v bool) *bool   { return &v }

// projectContent returns just the assembly-relevant fields of a row so the
// round-trip assertion ignores identity/ledger columns (id, conversation,
// tokens, cost, window_state) that MessageToRow never sets — they are the
// caller's to stamp, not the bijection's.
func projectContent(m domain.Message) domain.Message {
	return domain.Message{
		Role:          m.Role,
		Content:       m.Content,
		Subtype:       m.Subtype,
		ToolCalls:     m.ToolCalls,
		ToolCallID:    m.ToolCallID,
		IsError:       m.IsError,
		Reasoning:     m.Reasoning,
		ContentBlocks: m.ContentBlocks,
	}
}

// bijectionCorpus is the round-trip corpus: every content shape the native
// loop persists, in the canonical row form MessageToRow produces (text in
// flat Content, non-text in ContentBlocks).
func bijectionCorpus() map[string]domain.Message {
	return map[string]domain.Message{
		"user text": {
			Role: "user", Subtype: "text", Content: "please fix the failing test",
		},
		"assistant text only": {
			Role: "assistant", Subtype: "text", Content: "On it.",
		},
		"assistant reasoning with signature": {
			Role: "assistant", Subtype: "text", Content: "The bug is a nil deref.",
			Reasoning: []domain.ReasoningDetail{{
				Index:     0,
				Type:      "reasoning.text",
				Text:      "Let me trace the call path...",
				Signature: "c2lnbmF0dXJlLWJsb2I=",
			}},
		},
		"assistant redacted reasoning": {
			Role: "assistant", Subtype: "text", Content: "Reasoning withheld.",
			Reasoning: []domain.ReasoningDetail{{
				Index: 0,
				Type:  "reasoning.encrypted",
				Data:  "ZW5jcnlwdGVkLXJlYXNvbmluZy1wYXlsb2Fk",
			}},
		},
		"assistant tool call": {
			Role: "assistant", Subtype: "tool_use",
			ToolCalls: []domain.ToolCall{{
				ID:    "toolu_01",
				Name:  "Read",
				Input: map[string]any{"path": "/etc/hosts"},
			}},
		},
		"assistant reasoning plus tool call plus text": {
			Role: "assistant", Subtype: "tool_use", Content: "Reading the file first.",
			Reasoning: []domain.ReasoningDetail{{
				Index:     0,
				Type:      "reasoning.text",
				Text:      "I should read before editing.",
				Signature: "c2ln",
			}},
			ToolCalls: []domain.ToolCall{{
				ID:    "toolu_02",
				Name:  "Read",
				Input: map[string]any{"path": "/main.go", "limit": float64(200)},
			}},
		},
		"tool result success": {
			Role: "tool", Subtype: "tool", ToolCallID: "toolu_01",
			Content: "127.0.0.1 localhost",
		},
		"tool result error": {
			Role: "tool", Subtype: "tool", ToolCallID: "toolu_03",
			Content: "exit status 1: file not found", IsError: true,
		},
		"tool result multi-block with image": {
			Role: "tool", Subtype: "tool", ToolCallID: "toolu_04",
			Content: "screenshot captured",
			ContentBlocks: []domain.ContentBlock{{
				Type:     domain.ContentBlockImage,
				ImageURL: &domain.ContentImageURL{URL: "data:image/png;base64,iVBORw0KGgo="},
			}},
		},
		"tool result image only": {
			Role: "tool", Subtype: "tool", ToolCallID: "toolu_05",
			ContentBlocks: []domain.ContentBlock{{
				Type:     domain.ContentBlockImage,
				ImageURL: &domain.ContentImageURL{URL: "data:image/png;base64,AAAA", Detail: "high"},
			}},
		},
	}
}

func TestBijection_RoundTrip(t *testing.T) {
	for name, row := range bijectionCorpus() {
		t.Run(name, func(t *testing.T) {
			msg, err := rowToMessage(row)
			if err != nil {
				t.Fatalf("rowToMessage: %v", err)
			}
			back, err := MessageToRow(msg)
			if err != nil {
				t.Fatalf("MessageToRow: %v", err)
			}
			want := projectContent(row)
			got := projectContent(back)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("round-trip mismatch:\n want %#v\n  got %#v", want, got)
			}
		})
	}
}

// TestBijection_SignatureSurvives pins the load-bearing fidelity: a reasoning
// block's signature and an encrypted block's data reach the bifrost message
// intact, so replay validates.
func TestBijection_SignatureSurvives(t *testing.T) {
	row := bijectionCorpus()["assistant reasoning with signature"]
	msg, err := rowToMessage(row)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ChatAssistantMessage == nil || len(msg.ReasoningDetails) != 1 {
		t.Fatalf("expected one reasoning detail, got %+v", msg.ChatAssistantMessage)
	}
	d := msg.ReasoningDetails[0]
	if d.Signature == nil || *d.Signature != "c2lnbmF0dXJlLWJsb2I=" {
		t.Fatalf("signature not carried: %+v", d.Signature)
	}

	enc := bijectionCorpus()["assistant redacted reasoning"]
	encMsg, _ := rowToMessage(enc)
	if encMsg.ChatAssistantMessage == nil {
		t.Fatal("redacted reasoning must attach an assistant payload")
	}
	ed := encMsg.ReasoningDetails[0]
	if ed.Type != schemas.BifrostReasoningDetailsTypeEncrypted {
		t.Fatalf("encrypted type not carried: %q", ed.Type)
	}
	if ed.Data == nil || *ed.Data == "" {
		t.Fatal("encrypted data not carried")
	}
}

// TestBijection_IsErrorSurvives pins the is_error mapping both directions: a
// failed tool result carries the marker, a successful one leaves it off (nil,
// not false) so omitempty keeps it off the OpenAI wire.
func TestBijection_IsErrorSurvives(t *testing.T) {
	errRow := bijectionCorpus()["tool result error"]
	msg, _ := rowToMessage(errRow)
	if msg.ChatToolMessage == nil || msg.IsError == nil || !*msg.IsError {
		t.Fatalf("is_error must be set true on an errored tool result: %+v", msg.ChatToolMessage)
	}

	okRow := bijectionCorpus()["tool result success"]
	okMsg, _ := rowToMessage(okRow)
	if okMsg.IsError != nil {
		t.Fatalf("is_error must be nil on a successful tool result, got %v", *okMsg.IsError)
	}
}

// TestContentBlocks_UnsupportedProviderTypeDropped pins the deliberate
// asymmetry: a provider block type the domain can't model (here input_audio) is
// dropped on ingest, the representable blocks survive, and — critically — the
// resulting row still re-assembles without error, so one exotic block never
// bricks a conversation's continuation.
func TestContentBlocks_UnsupportedProviderTypeDropped(t *testing.T) {
	msg := schemas.ChatMessage{
		Role:            schemas.ChatMessageRoleTool,
		ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: strPtr("t1")},
		Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
			{Type: schemas.ChatContentBlockTypeText, Text: strPtr("see attached")},
			{Type: schemas.ChatContentBlockTypeInputAudio, InputAudio: &schemas.ChatInputAudio{Data: "AAAA"}},
			{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: "data:image/png;base64,AA"}},
		}},
	}
	row, err := MessageToRow(msg)
	if err != nil {
		t.Fatalf("MessageToRow: %v", err)
	}
	if row.Content != "see attached" {
		t.Fatalf("text content must survive, got %q", row.Content)
	}
	if len(row.ContentBlocks) != 1 || row.ContentBlocks[0].Type != domain.ContentBlockImage {
		t.Fatalf("audio must be dropped and image kept, got %+v", row.ContentBlocks)
	}
	// The row that carried an unsupported block must still re-assemble — the
	// bug this guards against is a kept stub that the send path then rejects.
	if _, err := rowToMessage(row); err != nil {
		t.Fatalf("re-assembly must not error after an unsupported block was dropped: %v", err)
	}
}

// TestContentBlocksToSchema_RejectsOutOfVocabDomainType pins the strict half:
// the send path treats an out-of-vocabulary domain block type as an invariant
// violation and fails loud (it can only occur on a genuine bug now that ingest
// never produces one).
func TestContentBlocksToSchema_RejectsOutOfVocabDomainType(t *testing.T) {
	if _, err := contentBlocksToSchema([]domain.ContentBlock{{Type: "input_audio"}}); err == nil {
		t.Fatal("an out-of-vocabulary domain block type must error on the send path")
	}
}

func TestRowsToMessages_SeqOrdering(t *testing.T) {
	// Ids arrive out of order; seq overrides. A row at seq 1.5 lands between
	// id 1 and id 2. Roles alternate so adjacent-user consolidation (which
	// runs after ordering) leaves each message its own — ordering is what
	// this pins, and TestConsolidateAdjacentUsers covers the packing.
	rows := []domain.Message{
		{ID: 2, Role: "user", Content: "second"},
		{ID: 1, Role: "user", Content: "first"},
		{ID: 3, Role: "assistant", Content: "third", Seq: f64(1.5)},
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(msgs))
	for i, m := range msgs {
		got[i] = *m.Content.ContentStr
	}
	want := []string{"first", "third", "second"} // id1(1), seq1.5, id2(2)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seq ordering: want %v got %v", want, got)
	}
}

func TestRowsToMessages_UndeliveredExcludedByDefault(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "delivered"},
		{ID: 2, Role: "user", Content: "pending", Delivered: boolPtr(false)},
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || *msgs[0].Content.ContentStr != "delivered" {
		t.Fatalf("undelivered row must be excluded by default: %+v", msgs)
	}

	// With pending rows folded in, both user rows are present — packed into
	// one message by adjacent-user consolidation, which is the shape the
	// wire needs.
	withPending, err := RowsToMessages(rows, AssemblyOptions{IncludeUndelivered: true, NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withPending) != 1 {
		t.Fatalf("two adjacent user rows must pack into one message: %+v", withPending)
	}
	blocks := withPending[0].Content.ContentBlocks
	if len(blocks) != 2 || *blocks[0].Text != "delivered" || *blocks[1].Text != "pending" {
		t.Fatalf("IncludeUndelivered must fold pending rows in, in order: %+v", blocks)
	}
}

func TestRowsToMessages_InactiveExcluded(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "kept"},
		{ID: 2, Role: "assistant", Content: "compacted away", WindowState: domain.MessageWindowInactive},
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || *msgs[0].Content.ContentStr != "kept" {
		t.Fatalf("inactive row must be excluded: %+v", msgs)
	}
}

func TestRowsToMessages_ElidedStub(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "assistant", Content: "keep", ToolCalls: []domain.ToolCall{{ID: "t1", Name: "Read", Input: map[string]any{"path": "/x"}}}},
		{ID: 2, Role: "tool", ToolCallID: "t1", Content: "a very large tool result that got elided", IsError: true, WindowState: domain.MessageWindowElided},
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	stub := msgs[1]
	if stub.Role != schemas.ChatMessageRoleTool {
		t.Fatalf("elided tool row must stay a tool message: %q", stub.Role)
	}
	if stub.Content == nil || stub.Content.ContentStr == nil || *stub.Content.ContentStr != elidedStubText {
		t.Fatalf("elided row must render the fixed stub, got %+v", stub.Content)
	}
	// is_error and tool_call_id survive the stub so the transcript stays valid.
	if stub.ChatToolMessage == nil || stub.IsError == nil || !*stub.IsError {
		t.Fatalf("is_error must survive elision: %+v", stub.ChatToolMessage)
	}
	if stub.ToolCallID == nil || *stub.ToolCallID != "t1" {
		t.Fatalf("tool_call_id must survive elision: %+v", stub.ChatToolMessage)
	}
}

func TestRowsToMessages_MovingCacheBreakpoint(t *testing.T) {
	rows := []domain.Message{
		{ID: 1, Role: "user", Content: "hello"},
		{ID: 2, Role: "assistant", Content: "hi"},
		{ID: 3, Role: "user", Content: "the newest turn"},
	}
	msgs, err := RowsToMessages(rows, AssemblyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	last := msgs[len(msgs)-1]
	if last.Content == nil || len(last.Content.ContentBlocks) != 1 {
		t.Fatalf("final message should be promoted to a single block, got %+v", last.Content)
	}
	if last.Content.ContentBlocks[0].CacheControl == nil {
		t.Fatal("final message's last block must carry the moving cache breakpoint")
	}
	if last.Content.ContentBlocks[0].CacheControl.Type != schemas.CacheControlTypeEphemeral {
		t.Fatalf("cache breakpoint must be ephemeral, got %q", last.Content.ContentBlocks[0].CacheControl.Type)
	}
	// Earlier messages must NOT carry a breakpoint.
	if msgs[0].Content != nil && len(msgs[0].Content.ContentBlocks) > 0 && msgs[0].Content.ContentBlocks[0].CacheControl != nil {
		t.Fatal("only the final message should carry the moving breakpoint")
	}

	// Opting out leaves the final message as plain string content.
	plain, _ := RowsToMessages(rows, AssemblyOptions{NoCacheBreakpoint: true})
	if plain[len(plain)-1].Content.ContentStr == nil {
		t.Fatal("NoCacheBreakpoint must leave string content unpromoted")
	}
}
