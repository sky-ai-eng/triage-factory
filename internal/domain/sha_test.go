package domain

import "testing"

func TestShortSHA(t *testing.T) {
	if got := ShortSHA("abcdef0123456789"); got != "abcdef012345" {
		t.Errorf("ShortSHA truncate: got %q want abcdef012345", got)
	}
	if got := ShortSHA("abc"); got != "abc" {
		t.Errorf("ShortSHA short value: got %q want abc", got)
	}
	if got := ShortSHA(""); got != "" {
		t.Errorf("ShortSHA empty: got %q want empty", got)
	}
}
