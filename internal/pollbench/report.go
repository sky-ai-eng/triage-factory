package pollbench

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"
)

// Render writes the human-readable metrics report. The per-cycle table is
// the payload; the trailing lines summarize sync cost, conditional-request
// savings, memory, and the sanity verdict.
func (r *Result) Render(w io.Writer) {
	fmt.Fprintf(w, "repos=%s  open PRs=%s (max/repo=%s)  expected cold events=%s\n\n",
		comma(r.Repos), comma(r.TotalPRs), comma(r.MaxOpenPRs), comma(r.ExpectedColdEvents))

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "cycle\twall\treq\tlist\tpages\t304\tgraphql\tteams\tratelimited\tevents\tentities\tallocs")
	for _, c := range r.Cycles {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Label, fmtDur(c.Wall),
			comma(c.Requests.Total), comma(c.Requests.PullsList), comma(c.Requests.PullsPages),
			comma(c.Requests.Pulls304), comma(c.Requests.GraphQL), comma(c.Requests.Teams),
			comma(c.Requests.RateLimited),
			comma(c.Events), comma(c.Entities), fmtBytes(int64(c.AllocBytes)))
	}
	_ = tw.Flush()

	if r.ColdCycles > 0 {
		fmt.Fprintf(w, "\ncold sync: complete in %d cycle(s), %s wall (incl. rate-limit waits), %s events\n",
			r.ColdCycles, fmtDur(r.ColdSyncWall), comma(r.ColdEvents))
	} else {
		fmt.Fprintf(w, "\ncold sync: INCOMPLETE (cycle cap reached), %s wall, %s events\n",
			fmtDur(r.ColdSyncWall), comma(r.ColdEvents))
	}

	if cond, hits := r.Warm304(); cond > 0 {
		fmt.Fprintf(w, "warm 304 hit rate: %.0f%% (%d/%d conditional listings)\n",
			100*float64(hits)/float64(cond), hits, cond)
	}

	if r.PeakRSSBytes > 0 {
		fmt.Fprintf(w, "peak RSS: %s\n", fmtBytes(r.PeakRSSBytes))
	} else {
		fmt.Fprintf(w, "peak RSS: n/a on this platform\n")
	}

	if len(r.EventTypes) > 0 {
		type kv struct {
			k string
			v int
		}
		var types []kv
		for k, v := range r.EventTypes {
			types = append(types, kv{k, v})
		}
		sort.Slice(types, func(i, j int) bool {
			if types[i].v != types[j].v {
				return types[i].v > types[j].v
			}
			return types[i].k < types[j].k
		})
		fmt.Fprint(w, "events by type:")
		for _, t := range types {
			fmt.Fprintf(w, " %s=%s", t.k, comma(t.v))
		}
		fmt.Fprintln(w)
	}

	for _, c := range r.Cycles {
		for _, e := range c.Errors {
			fmt.Fprintf(w, "cycle error (%s): %s\n", c.Label, e)
		}
	}

	if len(r.SanityErrors) == 0 {
		fmt.Fprintln(w, "sanity: OK")
	} else {
		for _, s := range r.SanityErrors {
			fmt.Fprintf(w, "sanity: FAIL — %s\n", s)
		}
	}
}

// Warm304 sums warm-cycle conditional page-1 listings and how many of them
// were 304 hits — the ETag-savings measurement (also asserted by -check).
func (r *Result) Warm304() (conditional, hits int) {
	for _, c := range r.Cycles {
		if len(c.Label) >= 4 && c.Label[:4] == "warm" {
			conditional += c.Requests.PullsList + c.Requests.Pulls304
			hits += c.Requests.Pulls304
		}
	}
	return conditional, hits
}

func comma(n int) string {
	s := strconv.Itoa(n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var out []byte
	for i, ch := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, ch)
	}
	return string(out)
}

func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02.0fs", int(d.Minutes()), d.Seconds()-60*float64(int(d.Minutes())))
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
