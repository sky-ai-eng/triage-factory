package fsutil

import (
	"errors"
	"testing"
)

func TestRenameReplace_RetriesTransientFailures(t *testing.T) {
	prevRename, prevTransient := renameFile, renameTransient
	t.Cleanup(func() { renameFile, renameTransient = prevRename, prevTransient })
	renameTransient = func(error) bool { return true }

	calls := 0
	renameFile = func(_, _ string) error {
		calls++
		if calls < 3 {
			return errors.New("sharing violation")
		}
		return nil
	}
	if err := RenameReplace("a", "b"); err != nil || calls != 3 {
		t.Fatalf("err = %v after %d calls; want success on the third", err, calls)
	}

	calls = 0
	renameFile = func(_, _ string) error { calls++; return errors.New("still busy") }
	if err := RenameReplace("a", "b"); err == nil || calls != renameAttempts {
		t.Fatalf("err = %v after %d calls; want failure after %d", err, calls, renameAttempts)
	}
}

func TestRenameReplace_DoesNotRetryOtherFailures(t *testing.T) {
	prevRename, prevTransient := renameFile, renameTransient
	t.Cleanup(func() { renameFile, renameTransient = prevRename, prevTransient })
	renameTransient = func(error) bool { return false }

	calls := 0
	renameFile = func(_, _ string) error { calls++; return errors.New("permanent") }
	if err := RenameReplace("a", "b"); err == nil || calls != 1 {
		t.Fatalf("err = %v after %d calls; want one call", err, calls)
	}
}
