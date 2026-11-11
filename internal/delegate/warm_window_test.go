package delegate

import (
	"testing"
	"time"
)

func TestParseWarmWindow(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: DefaultWarmWindow},
		{raw: "0", want: 0},
		{raw: "90s", want: 90 * time.Second},
		{raw: "-1m", wantErr: true},
		{raw: "ten minutes", wantErr: true},
	} {
		got, err := ParseWarmWindow(tc.raw)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ParseWarmWindow(%q) = (%v, %v), want (%v, error %v)", tc.raw, got, err, tc.want, tc.wantErr)
		}
	}
}
