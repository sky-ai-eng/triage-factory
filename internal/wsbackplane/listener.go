package wsbackplane

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// RunPublicListener opens a dedicated (session-mode, non-pooled) LISTEN
// connection for tf_ws + tf_ctl and dispatches every notification until
// ctx is cancelled. Every control pod runs this — it is what makes
// Broadcast/CloseUserConnections cross-pod (TFAC-584).
//
// Both channels share one physical connection deliberately: they're
// both low-volume-by-design (tf_ctl is a hard rule other channels must
// never ride — see ChannelBus's doc comment; tf_ws's own volume is the
// one real-volume channel in the fabric, but the FIFO cost of an
// occasional tf_ctl kick sitting behind a burst of tf_ws notifications
// is the ordering guarantee this scope, not tf_bus's — that separation
// is what RunBusListener is for). Per-channel relative order is
// preserved either way: a single connection's NOTIFY stream is a strict
// FIFO regardless of how many channels are LISTENed on it, and this
// function dispatches from one goroutine, never fanning a notification's
// handling out to another (spec §5's ordering rule).
func (b *Backplane) RunPublicListener(ctx context.Context) {
	listenLoop(ctx, b.directDSN, []string{ChannelWS, ChannelCtl}, "public", b.dispatchPublic)
}

// RunBusListener opens a SEPARATE dedicated LISTEN connection for tf_bus
// and dispatches brain-bound run-sentinel envelopes until ctx is
// cancelled — republishing each non-self-origin one onto localPublish
// (the brain's own in-process eventbus). Only the brain calls this; its
// caller starts/stops it alongside every other brain-gated subsystem
// (today that's `a.plan.brain`, single-control-only until TFAC-583's
// lease lands — see internal/app/startup.go).
//
// A dedicated connection, not a shared one with RunPublicListener, is
// the whole point: run-sentinel volume tracks agent activity and must
// never be able to head-of-line-block a tf_ctl kick or cancel sitting
// behind it in the same connection's NOTIFY FIFO (spec §5.3).
func (b *Backplane) RunBusListener(ctx context.Context, localPublish func(domain.Event)) {
	listenLoop(ctx, b.directDSN, []string{ChannelBus}, "brain", func(_ string, payload string) {
		b.handleBusNotification(payload, localPublish)
	})
}

func (b *Backplane) dispatchPublic(channel, payload string) {
	switch channel {
	case ChannelWS:
		b.handleWSNotification(payload)
	case ChannelCtl:
		b.handleCtlNotification(payload)
	default:
		backplaneLog.Warn("public listener: notification on unexpected channel; dropping", "channel", channel)
	}
}

// listenLoop opens a dedicated connection to dsn, LISTENs on every
// channel, and dispatches each notification to handle synchronously —
// one goroutine, in the order Postgres delivered them — until ctx is
// cancelled. On any connection error it reconnects with exponential
// backoff (capped at maxListenBackoff), logging Warn below
// listenFailureLogThreshold consecutive failures and Error at/above it:
// a pod that can't LISTEN is degraded to local-only fan-out, never
// crashed, but an operator needs to be able to notice the degradation
// (TFAC-584's "keep serving, log loudly" contract).
func listenLoop(ctx context.Context, dsn string, channels []string, name string, handle func(channel, payload string)) {
	if dsn == "" {
		backplaneLog.Warn("listener not started: no direct DSN configured", "listener", name)
		return
	}
	backoff := initialListenBackoff
	consecutiveFailures := 0
	for ctx.Err() == nil {
		connectedAt := time.Now()
		err := listenOnce(ctx, dsn, channels, handle)
		if ctx.Err() != nil {
			return
		}
		// A connection that stayed up longer than the backoff ceiling
		// proved the backplane is healthy again — reset so a later,
		// unrelated blip starts back at the short initial backoff and the
		// Warn/Error threshold instead of staying escalated forever.
		if time.Since(connectedAt) >= maxListenBackoff {
			backoff = initialListenBackoff
			consecutiveFailures = 0
		}
		consecutiveFailures++
		if consecutiveFailures >= listenFailureLogThreshold {
			backplaneLog.Error("LISTEN connection degraded after repeated failures; this pod is running local-only fan-out until it reconnects",
				"listener", name, "channels", channels, "consecutive_failures", consecutiveFailures, "error", err)
		} else {
			backplaneLog.Warn("LISTEN connection failed; retrying",
				"listener", name, "channels", channels, "error", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxListenBackoff {
			backoff = maxListenBackoff
		}
	}
}

// listenOnce opens one connection, issues LISTEN for every channel, and
// blocks dispatching notifications until the connection errors or ctx is
// cancelled. Always returns a non-nil error except on a clean ctx
// cancellation (in which case the caller's ctx.Err() check exits the
// retry loop before looking at it).
func listenOnce(ctx context.Context, dsn string, channels []string, handle func(channel, payload string)) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() {
		// Best-effort: WaitForNotification below already failed or ctx is
		// cancelled by the time we reach this, so a close error here isn't
		// actionable — the OS reclaims the socket either way.
		_ = conn.Close(context.Background())
	}()

	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return fmt.Errorf("listen %s: %w", ch, err)
		}
	}
	// Reset backoff bookkeeping happens in the caller on any successful
	// dispatch loop entry — reaching here means LISTEN succeeded.
	for {
		notif, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("wait for notification: %w", err)
		}
		handle(notif.Channel, notif.Payload)
	}
}
