package slack

// exec_types.go is the wire contract between the CLI verbs (cli.go, running
// in the agent's process — sandboxed in multi mode) and the host handler
// (exec_host.go, running host-side). Every type here round-trips through
// agenthost.Client.CallExtension's single JSON-frame-in/frame-out contract
// (cmd/exec/agenthost/extension.go), so both ends import this same package
// and never hand-decode each other's shape.
//
// slackExecMaxFileBytes bounds a file crossing the RPC in either direction
// (send --attach-file, download): a file's bytes cross as base64 inside the
// JSON frame (the sandbox holds no Slack credential to fetch/upload directly
// — see the package doc), and the frame itself is capped at 16 MiB
// (agenthost.maxFrameSize). 8 MiB of raw bytes base64-encodes to ~10.7 MiB,
// leaving headroom for the JSON envelope around it.
const slackExecMaxFileBytes = 8 << 20

// slackSendArgs is `exec slack send`'s wire shape. Body and the attachment
// are independently optional — send allows a file-only message — but never
// both empty (the CLI rejects that before calling out). AttachName/
// AttachBase64 are set together or not at all.
type slackSendArgs struct {
	Channel      string `json:"channel"`
	ThreadTS     string `json:"thread_ts,omitempty"`
	Body         string `json:"body,omitempty"`
	AttachName   string `json:"attach_name,omitempty"`
	AttachBase64 string `json:"attach_base64,omitempty"`
}

// slackSendResult is `send`'s success shape: the channel and the ts of the
// posted message (the root ts on a channel-root post, a reply ts otherwise)
// — the coordinates a later `edit`/`react` call needs.
type slackSendResult struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// slackEditArgs is `exec slack edit`'s wire shape — chat.update physics:
// only the bot's own messages, addressed by (channel, ts).
type slackEditArgs struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Body    string `json:"body"`
}

// slackEditResult echoes the edited message's coordinates back, mirroring
// slackSendResult's shape so the CLI's printJSON output is uniform across
// both verbs.
type slackEditResult struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// slackReactArgs is `exec slack react`'s wire shape.
type slackReactArgs struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Emoji   string `json:"emoji"`
}

// slackReadThreadArgs is `exec slack read thread`'s wire shape — replies
// under a root message via conversations.replies.
type slackReadThreadArgs struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Limit   int    `json:"limit,omitempty"`
}

// slackReadChannelArgs is `exec slack read channel`'s wire shape. Two modes,
// mutually exclusive at the CLI: a plain latest-N read (Limit only), or an
// anchored N-prior/N-following window around TS. NumPrior/NumFollowing are
// independently optional within the anchored mode (either alone is valid —
// "just what came after this message", say).
type slackReadChannelArgs struct {
	Channel      string `json:"channel"`
	TS           string `json:"ts,omitempty"`
	NumPrior     int    `json:"num_prior,omitempty"`
	NumFollowing int    `json:"num_following,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

// slackMessageView is one message in a read verb's output array — sender
// name resolution is best-effort (see viewMessages in exec_host.go),
// so SenderName is "" rather than failing the whole read when it can't
// resolve.
type slackMessageView struct {
	TS         string             `json:"ts"`
	ThreadTS   string             `json:"thread_ts,omitempty"`
	SenderID   string             `json:"sender_id,omitempty"`
	SenderName string             `json:"sender_name,omitempty"`
	Text       string             `json:"text"`
	Files      []slackFileRefView `json:"files,omitempty"`
}

// slackFileRefView is one file attached to a read message.
type slackFileRefView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// slackDownloadArgs is `exec slack download`'s wire shape.
type slackDownloadArgs struct {
	FileID string `json:"file_id"`
}

// slackDownloadResult carries the downloaded file's name + base64 body back
// to the CLI, which decodes and writes it to the agent's cwd (or --out) and
// prints only {"path":...} — the base64 payload never reaches stdout.
type slackDownloadResult struct {
	Name   string `json:"name"`
	Base64 string `json:"base64"`
}
