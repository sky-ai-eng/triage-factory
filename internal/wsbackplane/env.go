package wsbackplane

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Channel names for the three LISTEN/NOTIFY channels this package owns
// (docs/specs/horizontal-scaling/README.md §5). tf_wake (control→executor
// claim nudges) belongs to the run-queue dispatcher, not here.
const (
	// ChannelWS carries websocket.Hub.Broadcast envelopes: every control
	// pod LISTENs and fans a received event into its own local sockets.
	ChannelWS = "tf_ws"
	// ChannelCtl carries the cross-pod session kick (TFAC-584) today;
	// TFAC-585's run_signals share it (both are low-volume by design —
	// high-volume traffic must never ride this channel, see ChannelBus).
	ChannelCtl = "tf_ctl"
	// ChannelBus carries brain-bound run-sentinel relay envelopes
	// (TFAC-592). Only the brain LISTENs; deliberately separate from
	// ChannelCtl so a sentinel burst can never head-of-line-block a kick
	// or a cancel behind it in a single connection's NOTIFY FIFO.
	ChannelBus = "tf_bus"
)

// notifyHardCapBytes is a defensive margin under Postgres's ~8000-byte
// NOTIFY payload ceiling. The envelope wrapper (origin id, JSON
// punctuation) adds a little over the raw event size, so the configured
// inline threshold is clamped well below the true server-side limit.
const notifyHardCapBytes = 7800

// Tunable defaults (spec §11 decision 5 — "env-tunable defaults").
const (
	DefaultInlineMaxBytes = 6 * 1024

	DefaultOutboxTTL          = 60 * time.Second
	DefaultOutboxReapInterval = 20 * time.Second

	DefaultPresenceHeartbeatInterval = 15 * time.Second
	DefaultPresenceLiveWindow        = 45 * time.Second
	DefaultPresenceReapInterval      = 60 * time.Second
	// DefaultPresenceRowTTL is deliberately much larger than
	// DefaultPresenceLiveWindow: the live window governs whether a row
	// counts as "present" for reads (PresentFor), the row TTL governs
	// when the reaper physically deletes it for storage hygiene — a row
	// this old has been unambiguously dead (and unreachable by any future
	// heartbeat, since a reconnect always mints a fresh conn_id) for a
	// comfortable margin past when it stopped mattering to any read.
	DefaultPresenceRowTTL = 5 * time.Minute

	DefaultRevalidateInterval = 60 * time.Second

	// initialListenBackoff / maxListenBackoff bound the LISTEN connection's
	// reconnect backoff (TFAC-584: "reconnect with backoff + a loud log
	// after N consecutive failures").
	initialListenBackoff = 1 * time.Second
	maxListenBackoff     = 30 * time.Second
	// listenFailureLogThreshold is the consecutive-failure count at which
	// the reconnect loop escalates from Warn to Error — "a pod that can't
	// LISTEN is degraded... but must keep serving," so this is a loud
	// signal, not a fatal one.
	listenFailureLogThreshold = 5
)

// InlineMaxBytesFromEnv resolves TF_WS_INLINE_MAX_B: the largest tf_ws
// envelope that rides inline on the NOTIFY payload before falling back to
// a ws_outbox row + ref. An invalid or out-of-range value logs and falls
// back to the default rather than bricking boot (matches
// ParseMaxConcurrentRuns's convention in internal/delegate).
func InlineMaxBytesFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("TF_WS_INLINE_MAX_B"))
	if raw == "" {
		return DefaultInlineMaxBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		backplaneLog.Warn("invalid TF_WS_INLINE_MAX_B (want a positive integer); using default",
			"value", raw, "default", DefaultInlineMaxBytes)
		return DefaultInlineMaxBytes
	}
	if n > notifyHardCapBytes {
		backplaneLog.Warn("TF_WS_INLINE_MAX_B exceeds Postgres's NOTIFY payload ceiling; clamping",
			"value", n, "ceiling", notifyHardCapBytes)
		return notifyHardCapBytes
	}
	return n
}

// durationSecondsFromEnv resolves an env var holding a whole number of
// seconds, falling back to def on an empty, invalid, or non-positive
// value (logged as a warning — never a boot failure).
func durationSecondsFromEnv(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		backplaneLog.Warn("invalid duration env var (want a positive integer number of seconds); using default",
			"var", name, "value", raw, "default", def)
		return def
	}
	return time.Duration(n) * time.Second
}

// OutboxTTLFromEnv resolves TF_WS_OUTBOX_TTL_SECONDS.
func OutboxTTLFromEnv() time.Duration {
	return durationSecondsFromEnv("TF_WS_OUTBOX_TTL_SECONDS", DefaultOutboxTTL)
}

// PresenceHeartbeatIntervalFromEnv resolves TF_WS_PRESENCE_HEARTBEAT_SECONDS.
func PresenceHeartbeatIntervalFromEnv() time.Duration {
	return durationSecondsFromEnv("TF_WS_PRESENCE_HEARTBEAT_SECONDS", DefaultPresenceHeartbeatInterval)
}

// PresenceLiveWindowFromEnv resolves TF_WS_PRESENCE_TTL_SECONDS — a
// ws_presence row younger than this is considered live.
func PresenceLiveWindowFromEnv() time.Duration {
	return durationSecondsFromEnv("TF_WS_PRESENCE_TTL_SECONDS", DefaultPresenceLiveWindow)
}

// PresenceRowTTLFromEnv resolves TF_WS_PRESENCE_ROW_TTL_SECONDS — how
// long RunPresenceReaper keeps a ws_presence row before deleting it.
// Deliberately a separate knob from TF_WS_PRESENCE_TTL_SECONDS (the
// read-time live window): this one only bounds storage growth from
// reconnect churn, never PresentFor's answer — see DefaultPresenceRowTTL.
func PresenceRowTTLFromEnv() time.Duration {
	return durationSecondsFromEnv("TF_WS_PRESENCE_ROW_TTL_SECONDS", DefaultPresenceRowTTL)
}

// RevalidateIntervalFromEnv resolves TF_WS_SESSION_REVALIDATE_SECONDS —
// the cross-pod kick's backstop timer (see websocket.Hub.RevalidateSessions).
func RevalidateIntervalFromEnv() time.Duration {
	return durationSecondsFromEnv("TF_WS_SESSION_REVALIDATE_SECONDS", DefaultRevalidateInterval)
}

// DirectDSN resolves the session-mode Postgres DSN the LISTEN connections
// use: TF_DATABASE_DIRECT_URL when set, else TF_DATABASE_URL. The split
// exists so TFAC-307's future transaction-mode pooler can sit in front of
// TF_DATABASE_URL for pooled queries while TF_DATABASE_DIRECT_URL keeps
// pointing straight at Postgres for LISTEN — pooler-mode connections can't
// carry a LISTEN session (spec §5, TFAC-307 §2 interaction). Nothing pools
// TF_DATABASE_URL today, so defaulting to it is a no-op until 307 lands.
func DirectDSN() string {
	if v := strings.TrimSpace(os.Getenv("TF_DATABASE_DIRECT_URL")); v != "" {
		return v
	}
	return os.Getenv("TF_DATABASE_URL")
}
