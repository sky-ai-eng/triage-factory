package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestTxCause(t *testing.T) {
	live := context.Background()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Time{})
	defer cancelExpired()
	<-expired.Done()

	sentinel := errors.New("body refused")

	tests := []struct {
		name         string
		ctx          context.Context
		err          error
		wantOriginal bool
		wantCtxErr   error
	}{
		{"nil err passes through", canceled, nil, false, nil},
		{"live ctx passes through", live, sql.ErrTxDone, true, nil},
		{"tx done under canceled ctx carries both", canceled, sql.ErrTxDone, true, context.Canceled},
		{"body error under canceled ctx carries both", canceled, sentinel, true, context.Canceled},
		{"deadline stays a deadline", expired, sql.ErrTxDone, true, context.DeadlineExceeded},
		{"already wrapped is not double-wrapped", canceled, fmt.Errorf("set claims: %w", context.Canceled), true, context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TxCause(tc.ctx, tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if tc.wantOriginal && !errors.Is(got, tc.err) {
				t.Errorf("errors.Is(got, original) = false; got %v", got)
			}
			if tc.wantCtxErr != nil && !errors.Is(got, tc.wantCtxErr) {
				t.Errorf("errors.Is(got, %v) = false; got %v", tc.wantCtxErr, got)
			}
			if tc.wantCtxErr == nil && got != tc.err {
				t.Errorf("live ctx should return err unchanged; got %v", got)
			}
			if errors.Is(tc.err, context.Canceled) && got != tc.err {
				t.Errorf("err already carrying the ctx error should pass through; got %v", got)
			}
		})
	}
}
