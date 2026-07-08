package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// --- chat.postMessage / chat.update ---

// TestSlackChatPostMessage_PlainText covers the no-markdown path: text
// alone, no blocks key at all, and the posted ts is returned.
func TestSlackChatPostMessage_PlainText(t *testing.T) {
	var gotBody map[string]any
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Errorf("Authorization header = %q; want Bearer xoxb-test", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1700000000.000001"})
	})

	ts, err := slackChatPostMessage(context.Background(), srv.Client(), "xoxb-test",
		slackMessageParams{Channel: "C1", Text: "hello"})
	if err != nil {
		t.Fatalf("slackChatPostMessage: %v", err)
	}
	if ts != "1700000000.000001" {
		t.Errorf("ts = %q; want the posted ts", ts)
	}
	if gotBody["channel"] != "C1" || gotBody["text"] != "hello" {
		t.Errorf("body = %+v; want channel/text carried over", gotBody)
	}
	if _, ok := gotBody["blocks"]; ok {
		t.Errorf("body carries a blocks key with no MarkdownBody set: %+v", gotBody)
	}
}

// TestSlackChatPostMessage_MarkdownBody_SendsBlocksAndTextFallback pins the
// blocks+text pairing: a markdown block plus the plain-text fallback,
// never blocks alone.
func TestSlackChatPostMessage_MarkdownBody_SendsBlocksAndTextFallback(t *testing.T) {
	var gotBody map[string]any
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1700000000.000002"})
	})

	_, err := slackChatPostMessage(context.Background(), srv.Client(), "xoxb-test", slackMessageParams{
		Channel: "C1", ThreadTS: "1699999999.000001", Text: "fallback", MarkdownBody: "**bold**",
	})
	if err != nil {
		t.Fatalf("slackChatPostMessage: %v", err)
	}
	if gotBody["thread_ts"] != "1699999999.000001" {
		t.Errorf("thread_ts = %v; want the reply target", gotBody["thread_ts"])
	}
	if gotBody["text"] != "fallback" {
		t.Errorf("text = %v; want the plain-text fallback preserved", gotBody["text"])
	}
	blocks, ok := gotBody["blocks"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("blocks = %+v; want exactly one block", gotBody["blocks"])
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "markdown" || block["text"] != "**bold**" {
		t.Errorf("block = %+v; want {type:markdown, text:**bold**}", block)
	}
}

// TestSlackChatPostMessage_MarkdownTooLong_ReturnsTypedError_NoRequestSent
// pins the "don't truncate silently" contract: an over-cap body is
// rejected before any HTTP call is made.
func TestSlackChatPostMessage_MarkdownTooLong_ReturnsTypedError_NoRequestSent(t *testing.T) {
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.0"})
	})

	over := strings.Repeat("a", slackMarkdownBodyMaxRunes+1)
	_, err := slackChatPostMessage(context.Background(), srv.Client(), "xoxb-test",
		slackMessageParams{Channel: "C1", Text: "t", MarkdownBody: over})
	if err == nil {
		t.Fatal("slackChatPostMessage with an over-cap markdown body should error")
	}
	if !isSlackMarkdownTooLongError(err) {
		t.Errorf("err = %v (%T); want *slackMarkdownTooLongError", err, err)
	}
	if hits != 0 {
		t.Errorf("hits = %d; want 0 (validated before any request)", hits)
	}
}

// TestSlackChatUpdate_SendsTSNotThreadTS_AndPairsBlocksWithText pins
// chat.update's parameter shape: params.ThreadTS becomes the wire "ts" (the
// message being edited, not a thread-reply target), and text+blocks are
// always sent together.
func TestSlackChatUpdate_SendsTSNotThreadTS_AndPairsBlocksWithText(t *testing.T) {
	var gotBody map[string]any
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	err := slackChatUpdate(context.Background(), srv.Client(), "xoxb-test", slackMessageParams{
		Channel: "C1", ThreadTS: "1700000000.000003", Text: "edited", MarkdownBody: "*edited*",
	})
	if err != nil {
		t.Fatalf("slackChatUpdate: %v", err)
	}
	if gotBody["ts"] != "1700000000.000003" {
		t.Errorf("ts = %v; want the edited message's ts", gotBody["ts"])
	}
	if _, ok := gotBody["thread_ts"]; ok {
		t.Errorf("body carries a thread_ts key; chat.update has no such param: %+v", gotBody)
	}
	if gotBody["text"] != "edited" {
		t.Errorf("text = %v; want edited", gotBody["text"])
	}
	if _, ok := gotBody["blocks"]; !ok {
		t.Error("blocks missing when MarkdownBody was set — text-without-blocks would strip formatting")
	}
}

// TestSlackChatUpdate_NotOk surfaces a {ok:false} chat.update response as an
// error.
func TestSlackChatUpdate_NotOk(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
	})
	err := slackChatUpdate(context.Background(), srv.Client(), "xoxb-test", slackMessageParams{Channel: "C1", ThreadTS: "1.0", Text: "x"})
	if err == nil {
		t.Fatal("slackChatUpdate with {ok:false} should return an error")
	}
}

// --- reactions.add ---

// TestSlackReactionsAdd_AlreadyReacted_TreatedAsSuccess pins the
// idempotency contract: Slack's already_reacted error is the caller's
// desired end state, not a failure.
func TestSlackReactionsAdd_AlreadyReacted_TreatedAsSuccess(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "already_reacted"})
	})
	if err := slackReactionsAdd(context.Background(), srv.Client(), "xoxb-test", "C1", "1.0", "eyes"); err != nil {
		t.Errorf("slackReactionsAdd(already_reacted) = %v; want nil", err)
	}
}

// TestSlackReactionsAdd_OtherError_Surfaces confirms already_reacted is the
// ONLY error swallowed — every other failure still surfaces.
func TestSlackReactionsAdd_OtherError_Surfaces(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.FormValue("channel") != "C1" || r.FormValue("timestamp") != "1.0" || r.FormValue("name") != "eyes" {
			t.Errorf("form = %v; want channel/timestamp/name carried over", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_name"})
	})
	if err := slackReactionsAdd(context.Background(), srv.Client(), "xoxb-test", "C1", "1.0", "eyes"); err == nil {
		t.Fatal("slackReactionsAdd(invalid_name) should return an error")
	}
}

// --- conversations.replies ---

// fakePaginatedReplies serves conversations.replies across pageSize-sized
// pages, cursoring by an opaque integer offset, for exactly total messages
// — mirrors fakePaginatedChannels for the replies shape.
func fakePaginatedReplies(t *testing.T, total, pageSize int) *httptest.Server {
	t.Helper()
	return withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			var e error
			offset, e = strconv.Atoi(c)
			if e != nil {
				t.Fatalf("bad test cursor %q: %v", c, e)
			}
		}
		end := offset + pageSize
		if end > total {
			end = total
		}
		messages := make([]map[string]any, 0, end-offset)
		for i := offset; i < end; i++ {
			messages = append(messages, map[string]any{"user": "U1", "text": "msg", "ts": fmt.Sprintf("1600000000.%06d", i)})
		}
		next := ""
		if end < total {
			next = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "messages": messages, "has_more": next != "",
			"response_metadata": map[string]any{"next_cursor": next},
		})
	})
}

func withLoweredRepliesCap(t *testing.T, n int) {
	t.Helper()
	orig := slackConversationsRepliesCap
	slackConversationsRepliesCap = n
	t.Cleanup(func() { slackConversationsRepliesCap = orig })
}

// TestSlackConversationsReplies_PaginatesAcrossPages: fewer messages than
// the cap, spread across several pages — every page fetched, not
// truncated.
func TestSlackConversationsReplies_PaginatesAcrossPages(t *testing.T) {
	srv := fakePaginatedReplies(t, 25, 10)
	got, truncated, err := slackConversationsReplies(context.Background(), srv.Client(), "xoxb-test", "C1", "1600000000.000000", 0)
	if err != nil {
		t.Fatalf("slackConversationsReplies: %v", err)
	}
	if len(got) != 25 {
		t.Errorf("len(got) = %d; want 25 (all pages fetched)", len(got))
	}
	if truncated {
		t.Errorf("truncated = true; want false (well under the cap)")
	}
}

// TestSlackConversationsReplies_CapTruncates: more messages available than
// the (lowered) cap — result capped AND truncated=true.
func TestSlackConversationsReplies_CapTruncates(t *testing.T) {
	withLoweredRepliesCap(t, 15)
	srv := fakePaginatedReplies(t, 40, 10)
	got, truncated, err := slackConversationsReplies(context.Background(), srv.Client(), "xoxb-test", "C1", "1600000000.000000", 0)
	if err != nil {
		t.Fatalf("slackConversationsReplies: %v", err)
	}
	if len(got) != 15 {
		t.Errorf("len(got) = %d; want 15 (capped)", len(got))
	}
	if !truncated {
		t.Errorf("truncated = false; want true (more pages existed beyond the cap)")
	}
}

// TestSlackConversationsReplies_NotOk surfaces a {ok:false} response.
func TestSlackConversationsReplies_NotOk(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "thread_not_found"})
	})
	_, _, err := slackConversationsReplies(context.Background(), srv.Client(), "xoxb-test", "C1", "1.0", 0)
	if err == nil {
		t.Fatal("slackConversationsReplies with {ok:false} should return an error")
	}
}

// --- conversations.history ---

// TestSlackConversationsHistory_ParamsAndBounds pins the query-param
// encoding for the "two bounded calls around an anchor ts" pattern: latest/
// inclusive/limit for the before-context call.
func TestSlackConversationsHistory_ParamsAndBounds(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("channel") != "C1" {
			t.Errorf("channel = %q; want C1", q.Get("channel"))
		}
		if q.Get("latest") != "1700000000.000000" {
			t.Errorf("latest = %q; want the anchor ts", q.Get("latest"))
		}
		if q.Get("inclusive") != "" {
			t.Errorf("inclusive = %q; want unset (Inclusive=false)", q.Get("inclusive"))
		}
		if q.Get("limit") != "5" {
			t.Errorf("limit = %q; want 5", q.Get("limit"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "messages": []map[string]any{{"user": "U1", "text": "before", "ts": "1699999999.000001"}},
		})
	})

	got, err := slackConversationsHistory(context.Background(), srv.Client(), "xoxb-test", slackConversationsHistoryParams{
		Channel: "C1", Latest: "1700000000.000000", Limit: 5,
	})
	if err != nil {
		t.Fatalf("slackConversationsHistory: %v", err)
	}
	if len(got) != 1 || got[0].Text != "before" {
		t.Errorf("got = %+v; want one before-context message", got)
	}
}

// TestSlackConversationsHistory_OldestInclusive covers the after-context
// call shape: oldest + inclusive=true.
func TestSlackConversationsHistory_OldestInclusive(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("oldest") != "1700000000.000000" {
			t.Errorf("oldest = %q; want the anchor ts", q.Get("oldest"))
		}
		if q.Get("inclusive") != "true" {
			t.Errorf("inclusive = %q; want true", q.Get("inclusive"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{}})
	})
	if _, err := slackConversationsHistory(context.Background(), srv.Client(), "xoxb-test", slackConversationsHistoryParams{
		Channel: "C1", Oldest: "1700000000.000000", Inclusive: true,
	}); err != nil {
		t.Fatalf("slackConversationsHistory: %v", err)
	}
}

// --- chat.getPermalink ---

// TestSlackChatGetPermalink_GoldenDecode covers the happy path.
func TestSlackChatGetPermalink_GoldenDecode(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("channel") != "C1" || q.Get("message_ts") != "1700000000.000000" {
			t.Errorf("query = %v; want channel=C1 message_ts=1700000000.000000", q)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "permalink": "https://acme.slack.com/archives/C1/p1700000000000000"})
	})
	got, err := slackChatGetPermalink(context.Background(), srv.Client(), "xoxb-test", "C1", "1700000000.000000")
	if err != nil {
		t.Fatalf("slackChatGetPermalink: %v", err)
	}
	if got != "https://acme.slack.com/archives/C1/p1700000000000000" {
		t.Errorf("permalink = %q", got)
	}
}

// TestSlackChatGetPermalink_NotOk surfaces a {ok:false} response.
func TestSlackChatGetPermalink_NotOk(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
	})
	_, err := slackChatGetPermalink(context.Background(), srv.Client(), "xoxb-test", "C1", "1.0")
	if err == nil {
		t.Fatal("slackChatGetPermalink with {ok:false} should return an error")
	}
}

// --- assistant.threads.setStatus ---

// TestSlackAssistantSetStatus_GoldenParams pins the form encoding: status
// always present (even when clearing), loading_messages comma-joined.
func TestSlackAssistantSetStatus_GoldenParams(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.FormValue("channel_id") != "C1" || r.FormValue("thread_ts") != "1700000000.000000" {
			t.Errorf("form = %v; want channel_id/thread_ts carried over", r.Form)
		}
		if r.FormValue("status") != "is thinking..." {
			t.Errorf("status = %q", r.FormValue("status"))
		}
		if r.FormValue("loading_messages") != "step one,step two" {
			t.Errorf("loading_messages = %q; want comma-joined", r.FormValue("loading_messages"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	err := slackAssistantSetStatus(context.Background(), srv.Client(), "xoxb-test", "C1", "1700000000.000000",
		"is thinking...", []string{"step one", "step two"})
	if err != nil {
		t.Fatalf("slackAssistantSetStatus: %v", err)
	}
}

// TestSlackAssistantSetStatus_EmptyStatusClears pins that an empty status
// is still sent on the wire (Slack's own "empty clears" contract), not
// omitted.
func TestSlackAssistantSetStatus_EmptyStatusClears(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if _, present := r.Form["status"]; !present {
			t.Error("status key missing from the form; must be sent even when empty")
		}
		if r.FormValue("status") != "" {
			t.Errorf("status = %q; want empty", r.FormValue("status"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	if err := slackAssistantSetStatus(context.Background(), srv.Client(), "xoxb-test", "C1", "1.0", "", nil); err != nil {
		t.Fatalf("slackAssistantSetStatus: %v", err)
	}
}

// TestSlackAssistantSetStatus_TruncatesLoadingMessagesOver10 pins the
// documented 10-message cap.
func TestSlackAssistantSetStatus_TruncatesLoadingMessagesOver10(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		got := strings.Split(r.FormValue("loading_messages"), ",")
		if len(got) != slackAssistantLoadingMessagesMax {
			t.Errorf("loading_messages count = %d; want %d (truncated)", len(got), slackAssistantLoadingMessagesMax)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	many := make([]string, 15)
	for i := range many {
		many[i] = fmt.Sprintf("step %d", i)
	}
	if err := slackAssistantSetStatus(context.Background(), srv.Client(), "xoxb-test", "C1", "1.0", "working", many); err != nil {
		t.Fatalf("slackAssistantSetStatus: %v", err)
	}
}

// --- files.info / file download ---

// TestSlackFilesInfo_GoldenDecode covers the happy path.
func TestSlackFilesInfo_GoldenDecode(t *testing.T) {
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("file"); got != "F1" {
			t.Errorf("file query param = %q; want F1", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "file": map[string]any{
				"id": "F1", "name": "report.txt", "mimetype": "text/plain", "size": 42,
				"url_private": "https://files.slack.com/files-pri/T1-F1/report.txt",
			},
		})
	})
	got, err := slackFilesInfo(context.Background(), srv.Client(), "xoxb-test", "F1")
	if err != nil {
		t.Fatalf("slackFilesInfo: %v", err)
	}
	if got.ID != "F1" || got.Name != "report.txt" || got.Size != 42 || got.URLPrivate == "" {
		t.Errorf("got = %+v", got)
	}
}

// TestSlackFileDownload_StreamsBodyWithBearerAuth pins the download
// contract: Bearer auth header, streamed (not buffered-then-copied) body.
func TestSlackFileDownload_StreamsBodyWithBearerAuth(t *testing.T) {
	const payload = "the quick brown fox"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-test" {
			t.Errorf("Authorization header = %q; want Bearer xoxb-test", got)
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	if err := slackFileDownload(context.Background(), srv.Client(), "xoxb-test", srv.URL+"/files-pri/T1-F1/x.txt", &buf); err != nil {
		t.Fatalf("slackFileDownload: %v", err)
	}
	if buf.String() != payload {
		t.Errorf("downloaded body = %q; want %q", buf.String(), payload)
	}
}

// TestSlackFileDownload_HTTPError surfaces a non-2xx response as an error
// without writing partial garbage to w.
func TestSlackFileDownload_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	var buf bytes.Buffer
	err := slackFileDownload(context.Background(), srv.Client(), "xoxb-test", srv.URL+"/x.txt", &buf)
	if err == nil {
		t.Fatal("slackFileDownload with HTTP 403 should return an error")
	}
}

// --- files.getUploadURLExternal / files.completeUploadExternal ---

// TestSlackFilesUpload_FullFlow exercises the reserve→upload→complete
// sequence end to end against one fake server, pinning that thread_ts +
// channel land on the complete call so the file shares in-thread.
func TestSlackFilesUpload_FullFlow(t *testing.T) {
	var uploadedBytes []byte
	var completeBody map[string]any
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		if r.FormValue("filename") != "report.txt" || r.FormValue("length") != "11" {
			t.Errorf("form = %v; want filename=report.txt length=11", r.Form)
		}
		// upload_url is absolute in real Slack responses (a files.slack.com
		// host distinct from slack.com/api) — srv.URL here since this fake
		// server plays both roles.
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "upload_url": srv.URL + "/upload/v1/XYZ", "file_id": "F0NEW0001"})
	})
	mux.HandleFunc("/upload/v1/XYZ", func(w http.ResponseWriter, r *http.Request) {
		var err error
		uploadedBytes, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read uploaded bytes: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/files.completeUploadExternal", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&completeBody); err != nil {
			t.Fatalf("decode complete body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	orig := slackAPIBase
	slackAPIBase = srv.URL
	t.Cleanup(func() { slackAPIBase = orig })

	fileID, err := slackFilesUpload(context.Background(), srv.Client(), "xoxb-test", slackFileUploadParams{
		Filename: "report.txt", Length: 11, Title: "Report", Channel: "C1", ThreadTS: "1700000000.000000",
		Body: strings.NewReader("hello world"),
	})
	if err != nil {
		t.Fatalf("slackFilesUpload: %v", err)
	}
	if fileID != "F0NEW0001" {
		t.Errorf("fileID = %q; want F0NEW0001", fileID)
	}
	if string(uploadedBytes) != "hello world" {
		t.Errorf("uploaded bytes = %q; want hello world", uploadedBytes)
	}
	files, ok := completeBody["files"].([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("complete files = %+v; want one entry", completeBody["files"])
	}
	entry := files[0].(map[string]any)
	if entry["id"] != "F0NEW0001" || entry["title"] != "Report" {
		t.Errorf("file entry = %+v", entry)
	}
	if completeBody["channel_id"] != "C1" || completeBody["thread_ts"] != "1700000000.000000" {
		t.Errorf("complete body = %+v; want channel_id/thread_ts set so the file lands in-thread", completeBody)
	}
}

// TestSlackFilesUpload_GetUploadURLFails never reaches the upload or
// complete steps.
func TestSlackFilesUpload_GetUploadURLFails(t *testing.T) {
	hits := 0
	srv := withFakeSlackAPI(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_length"})
	})
	_, err := slackFilesUpload(context.Background(), srv.Client(), "xoxb-test", slackFileUploadParams{
		Filename: "x.txt", Length: 1, Channel: "C1", Body: strings.NewReader("x"),
	})
	if err == nil {
		t.Fatal("slackFilesUpload should fail when files.getUploadURLExternal fails")
	}
	if hits != 1 {
		t.Errorf("hits = %d; want 1 (never reached upload/complete)", hits)
	}
}
