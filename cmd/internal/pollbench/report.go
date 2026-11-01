package pollbench

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
)

// Render writes the human-readable metrics report. The per-cycle table is
// the payload; the trailing lines summarize sync cost, conditional-request
// savings, mergeability probing, memory, and the sanity verdict.
func (r *Result) Render(w io.Writer) {
	fmt.Fprintf(w, "repos=%s  open PRs=%s (max/repo=%s)  base branches=%s  expected cold events=%s\n",
		comma(r.Repos), comma(r.TotalPRs), comma(r.MaxOpenPRs), comma(r.BaseBranches), comma(r.ExpectedColdEvents))
	if r.TrackedPRs < r.TotalPRs {
		fmt.Fprintf(w, "tracked PRs=%s: the poll lists at most %s open PRs per repo, so %s are never discovered\n",
			comma(r.TrackedPRs), comma(ghclient.MaxOpenPRs), comma(r.TotalPRs-r.TrackedPRs))
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "cycle\twall\treq\tlist\tpages\t304\tgraphql\tpoints\tteams\trefs\tref304\tcompares\tpaths\tfiles\tratelimited\tmoves\tpushes\tprobes\tmerges\tevents\tentities\tallocs\tprobes by reason")
	for _, c := range r.Cycles {
		q := c.Requests
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Label, fmtDur(c.Wall),
			comma(q.Total), comma(q.PullsList), comma(q.PullsPages), comma(q.Pulls304),
			comma(q.GraphQL), comma(q.Points), comma(q.Teams),
			comma(q.Refs), comma(q.Refs304), comma(q.Compares), comma(q.PathReads), comma(q.FileLists),
			comma(q.RateLimited), comma(c.Moves), comma(c.HeadPushes),
			comma(c.ProbeTotal()), comma(q.TestMerges),
			comma(c.Events), comma(c.Entities), fmtBytes(int64(c.AllocBytes)), probeReasons(c.Probes))
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

	r.renderMergeability(w)

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

// renderMergeability summarizes what mergeability probing cost: the cold
// cycle against one probe per PR, the warm cycles' points and requests, and
// every test merge GitHub ran, by cause, beside what probing every open PR on
// every base move would have run.
func (r *Result) renderMergeability(w io.Writer) {
	if r.ColdCycles == 1 {
		c := r.Cycles[0]
		fmt.Fprintf(w, "cold cycle: %s probes for %s tracked PRs, %s path reads (%s GraphQL, %s REST), %s ref reads for %s base branches\n",
			comma(c.ProbeTotal()), comma(r.TrackedPRs), comma(c.Requests.PathReads+c.Requests.FileLists),
			comma(c.Requests.PathReads), comma(c.Requests.FileLists),
			comma(c.Requests.Refs+c.Requests.Refs304), comma(r.BaseBranches))
	}
	var warm Counters
	n := 0
	for _, c := range r.Cycles {
		if strings.HasPrefix(c.Label, "warm") {
			warm = countersSum(warm, c.Requests)
			n++
		}
	}
	if n > 0 {
		fmt.Fprintf(w, "warm cycles (%d): %s REST requests (%s of them 304s), %s GraphQL requests, %s points\n",
			n, comma(warm.REST()), comma(warm.Pulls304+warm.Refs304), comma(warm.GraphQL), comma(warm.Points))
	}
	if ws := r.World; ws.Moves > 0 || ws.HeadPushes > 0 {
		mean := 0.0
		if ws.Moves > 0 {
			mean = float64(ws.OverlapPRs) / float64(ws.Moves)
		}
		share := 0.0
		if ws.BaseOpenPRs > 0 {
			share = 100 * float64(ws.OverlapPRs) / float64(ws.BaseOpenPRs)
		}
		fmt.Fprintf(w, "base moves: %s (overlap set %.1f PRs per move, %.1f%% of the tracked PRs on the moved base); tracked head pushes: %s; PRs made conflicting: %s\n",
			comma(ws.Moves), mean, share, comma(ws.HeadPushes), comma(len(ws.Conflicted)))
	}
	t := r.Totals
	fmt.Fprintf(w, "test merges: %s — overlap set on a base move %s, head pushed %s, re-read after UNKNOWN %s; shortfall %s\n",
		comma(t.TestMerges), comma(t.TestMergesOverlap), comma(t.TestMergesHeadPushed), comma(t.TestMergesReread), comma(t.TestMergesUnaccounted))
	if r.World.Moves > 0 {
		fmt.Fprintf(w, "probing every tracked PR on every base move: %s test merges for the %s moves, against %s here (overlap set plus re-reads)\n",
			comma(r.World.BaseOpenPRs), comma(r.World.Moves), comma(t.TestMergesOverlap+t.TestMergesReread))
	}
}

// probeReasons renders a cycle's probes by reason in the vocabulary's order.
func probeReasons(probes map[string]int) string {
	var parts []string
	for _, reason := range domain.MergeabilityProbeReasons {
		if n := probes[reason]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%s", reason, comma(n)))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

// REST is how many REST requests the server answered (304s included).
func (c Counters) REST() int {
	return c.PullsList + c.PullsPages + c.Pulls304 + c.Teams + c.Refs + c.Refs304 + c.Compares + c.FileLists
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
