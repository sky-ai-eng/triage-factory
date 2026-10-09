package events

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCurrent_RegisteredViewRewritesOnlyItsType(t *testing.T) {
	const viewed = "test:current_view"
	RegisterCurrentView(viewed, func(_ context.Context, orgID, meta string) (string, error) {
		return strings.ReplaceAll(meta, "old", orgID), nil
	})
	t.Cleanup(func() { ResetCurrentView(viewed) })

	ctx := context.Background()
	if got, err := Current(ctx, "org-1", viewed, `{"channel":"old"}`); err != nil || got != `{"channel":"org-1"}` {
		t.Errorf("Current(viewed) = %q, %v; want the view's rewrite", got, err)
	}
	if got, err := Current(ctx, "org-1", "test:no_view", `{"channel":"old"}`); err != nil || got != `{"channel":"old"}` {
		t.Errorf("Current(unviewed) = %q, %v; want the metadata unchanged", got, err)
	}
	if got, err := Current(ctx, "org-1", viewed, ""); err != nil || got != "" {
		t.Errorf("Current(empty) = %q, %v; want empty without calling the view", got, err)
	}
}

func TestCurrent_ViewErrorReturned(t *testing.T) {
	const viewed = "test:current_view_error"
	boom := errors.New("boom")
	RegisterCurrentView(viewed, func(context.Context, string, string) (string, error) { return "", boom })
	t.Cleanup(func() { ResetCurrentView(viewed) })

	if _, err := Current(context.Background(), "org-1", viewed, `{}`); !errors.Is(err, boom) {
		t.Errorf("Current = %v; want the view's error", err)
	}
}

func TestRegisterCurrentView_DuplicatePanics(t *testing.T) {
	const viewed = "test:current_view_dup"
	view := func(_ context.Context, _, meta string) (string, error) { return meta, nil }
	RegisterCurrentView(viewed, view)
	t.Cleanup(func() { ResetCurrentView(viewed) })

	defer func() {
		if recover() == nil {
			t.Error("second RegisterCurrentView did not panic")
		}
	}()
	RegisterCurrentView(viewed, view)
}
