package wsbackplane

import (
	"context"
	"encoding/json"

	ws "github.com/coder/websocket"
)

// PublishKick notifies every OTHER control pod to close userID's local
// websocket connections (TFAC-584's cross-pod kick — logout, logout-all,
// membership removal). Call sites close THIS pod's matching local
// connections themselves first, via websocket.Hub.CloseUserConnections
// directly — PublishKick only carries the command to the rest of the
// fleet, and deliberately never calls CloseUserConnections itself: doing
// so would make the remote-dispatch path (handleCtlNotification, which
// DOES call CloseUserConnections) re-publish on every receipt, echoing
// the kick around the fleet forever. Keeping this method 100%
// Hub-unaware is what makes that impossible by construction.
//
// sid/orgID are optional narrowing filters — see
// websocket.Hub.CloseUserConnections's doc comment; pass "" for either to
// leave it unfiltered, exactly as that method does.
//
// Best-effort: a failed NOTIFY is logged and swallowed. The kick is
// security-relevant, so it is NOT tolerated as silently lossy the way
// tf_ws/tf_bus are — but the backstop for that is
// websocket.Hub.RevalidateSessions's slow timer, not a retry here (see
// that method's doc comment: "the timer is the guarantee; the NOTIFY is
// the latency optimization").
func (b *Backplane) PublishKick(ctx context.Context, userID, sid, orgID string, code int, reason string) {
	env := kickEnvelope{
		OriginInstanceID: b.originID,
		UserID:           userID,
		Sid:              sid,
		OrgID:            orgID,
		Code:             code,
		Reason:           reason,
	}
	if !b.notify(ctx, b.db, ChannelCtl, env) {
		backplaneLog.Warn("tf_ctl: kick notify failed; other pods rely on the session-revalidation backstop", "user", userID)
	}
}

// handleCtlNotification applies a received tf_ctl kick envelope: closes
// this pod's matching local sockets via CloseUserConnections directly —
// never PublishKick, which would re-broadcast it (see PublishKick's doc
// comment).
func (b *Backplane) handleCtlNotification(payload string) {
	var env kickEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		backplaneLog.Warn("tf_ctl: malformed envelope; dropping", "error", err)
		return
	}
	if env.OriginInstanceID == b.originID {
		// Already closed locally by the origin call site before it
		// published — see PublishKick's doc comment.
		return
	}
	n := b.hub.CloseUserConnections(env.UserID, env.Sid, env.OrgID, ws.StatusCode(env.Code), env.Reason)
	if n > 0 {
		backplaneLog.Info("tf_ctl: kicked local connections for a remote revoke", "user", env.UserID, "n", n)
	}
}
