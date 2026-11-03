package httpx

import (
	"errors"
	"strconv"
	"testing"
)

// pagedInts serves 0..n-1 per at a time, cursors being page numbers.
func pagedInts(n, per int, reads *int) func(string) ([]int, string, error) {
	return func(upstream string) ([]int, string, error) {
		*reads++
		page := 0
		if upstream != "" {
			page, _ = strconv.Atoi(upstream)
		}
		var rows []int
		for i := page * per; i < min((page+1)*per, n); i++ {
			rows = append(rows, i)
		}
		if (page+1)*per >= n {
			return rows, "", nil
		}
		return rows, strconv.Itoa(page + 1), nil
	}
}

func scanCursor(t *testing.T, raw string) ScanCursor {
	t.Helper()
	var v Validation
	c := ScanCursorFrom(&v, Page{Cursor: raw})
	if len(v.Items()) != 0 {
		t.Fatalf("cursor %q refused: %+v", raw, v.Items())
	}
	return c
}

func TestScanUpstream_FillsAcrossPagesAndResumesInside(t *testing.T) {
	multipleOf7 := func(i int) bool { return i%7 == 0 }
	var reads int
	read := pagedInts(50, 10, &reads)

	// 0, 7, 14: across two upstream pages, stopping inside the second.
	got, next, err := ScanUpstream(ScanCursor{}, 3, multipleOf7, read)
	if err != nil || len(got) != 3 || got[2] != 14 {
		t.Fatalf("page 1 = %v, %v", got, err)
	}
	// 21, 28, 35: resumed from the second page's position, not its start.
	got, next, err = ScanUpstream(scanCursor(t, next), 3, multipleOf7, read)
	if err != nil || len(got) != 3 || got[0] != 21 || got[2] != 35 {
		t.Fatalf("page 2 = %v, %v", got, err)
	}
	got, next, err = ScanUpstream(scanCursor(t, next), 3, multipleOf7, read)
	if err != nil || len(got) != 2 || got[0] != 42 || got[1] != 49 || next != "" {
		t.Fatalf("page 3 = %v (next %q), %v; want 42, 49 and the end", got, next, err)
	}
}

func TestScanUpstream_BoundedReadsAnswerAnEmptyPageThatResumes(t *testing.T) {
	last := func(i int) bool { return i == 499 }
	var reads int
	read := pagedInts(500, 10, &reads)

	got, next, err := ScanUpstream(ScanCursor{}, 5, last, read)
	if err != nil || len(got) != 0 || next == "" || reads != MaxScanReads {
		t.Fatalf("bounded page = %v (next %q, %d reads), %v; want none, a cursor and %d reads", got, next, reads, err, MaxScanReads)
	}
	for range 10 {
		if got, next, err = ScanUpstream(scanCursor(t, next), 5, last, read); err != nil || len(got) > 0 || next == "" {
			break
		}
	}
	if err != nil || len(got) != 1 || got[0] != 499 || next != "" {
		t.Fatalf("walk = %v (next %q), %v; want 499 and the end", got, next, err)
	}
}

func TestScanUpstream_APageThatShrankSkipsWhatItHas(t *testing.T) {
	all := func(int) bool { return true }
	read := func(string) ([]int, string, error) { return []int{1, 2}, "", nil }
	got, next, err := ScanUpstream(scanCursor(t, "5:"), 3, all, read)
	if err != nil || len(got) != 0 || next != "" {
		t.Fatalf("got %v (next %q), %v; want nothing left and the end", got, next, err)
	}
}

func TestScanUpstream_StalledCursorFails(t *testing.T) {
	read := func(upstream string) ([]int, string, error) { return []int{1}, "x", nil }
	if _, _, err := ScanUpstream(scanCursor(t, "0:x"), 5, func(int) bool { return false }, read); !errors.Is(err, errScanStalled) {
		t.Fatalf("err = %v, want a stalled cursor", err)
	}
}

func TestScanCursorFrom_RefusesAForeignCursor(t *testing.T) {
	for _, raw := range []string{"2", "x:abc", "-1:abc"} {
		var v Validation
		ScanCursorFrom(&v, Page{Cursor: raw})
		if fields(&v)["page_token"] != ReasonInvalidParam {
			t.Errorf("cursor %q: faults = %+v, want INVALID_PARAM on page_token", raw, v.Items())
		}
	}
}
