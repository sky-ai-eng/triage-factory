package domain

// SystemText is the system text a conversation's first claim sent, which
// every later claim sends again unchanged. The provider binds each thinking
// block to the system prompt it was produced under and refuses a request that
// replays one after the prompt changed, so a conversation learns of changes
// from appended system-update rows instead.
type SystemText struct {
	// Prompt is the shared framework prompt.
	Prompt string
	// Block is the conversation's own system block.
	Block string
	// Parts are the pieces Prompt and Block were composed from, in order.
	// A later claim compares its own composition against them. Nil for a
	// conversation whose parts were never recorded.
	Parts []SystemPart
}

// SystemPart is one named piece of a conversation's system text.
type SystemPart struct {
	// Key identifies the piece across claims and releases.
	Key string `json:"key"`
	// Name is how an update refers to the piece.
	Name string `json:"name"`
	Text string `json:"text"`
}

// SystemUpdateMetadataKey is the metadata key under which a system-update row
// carries every part of the composition it was made from.
const SystemUpdateMetadataKey = "system_parts"
