package slack

import (
	"context"
	"fmt"
	"strings"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entityscope"
)

func init() {
	entityscope.RegisterResolver("slack", resolveSlackThread)
}

// resolveSlackThread is the Slack source's entityscope.Resolver. A Slack
// entity references its channel's registry row, which only this package
// resolves, so core's touched-entity and produced-entity paths reach Slack
// threads through it. sourceID is the thread key an exec verb recorded as an
// artifact's or action's target (domain.SlackSourceID) — the boundary where a
// thread arrives as text — so it is split here into the channel id and the
// thread ts, and the thread is found by the channel's row and that ts. A
// channel id Slack has since replaced resolves to the channel's row; a channel
// TF holds no row for is an error, since every verb that records a thread
// authorized the channel first.
func resolveSlackThread(ctx context.Context, stores db.Stores, orgID, sourceID, kind, url string) (*domain.Entity, error) {
	channelID, threadTS, ok := strings.Cut(sourceID, "/")
	if !ok || channelID == "" || threadTS == "" {
		return nil, fmt.Errorf("slack: %q is not a thread key", sourceID)
	}
	bundle := slackstore.FromStores(stores)
	if bundle == nil {
		return nil, fmt.Errorf("slack: not available")
	}
	thread, err := bundle.Channels.FindOrCreateThreadSystem(ctx, orgID, channelID, threadTS, kind, "", url)
	if err != nil {
		return nil, fmt.Errorf("slack: thread %s: %w", sourceID, err)
	}
	entity, err := stores.Entities.GetSystem(ctx, orgID, thread.EntityID)
	if err != nil {
		return nil, err
	}
	if entity == nil {
		return nil, fmt.Errorf("slack: thread entity %s vanished after its find-or-create", thread.EntityID)
	}
	return entity, nil
}
