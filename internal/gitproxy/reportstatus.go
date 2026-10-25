package gitproxy

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// A git-receive-pack response says per ref whether the push landed: a
// report-status block of pkt-lines, "unpack ok" then one "ok <ref>" or
// "ng <ref> <reason>" per command, ended by a flush-pkt. GitHub refuses a ref
// (branch protection, a ruleset's GH013, a GHES pre-receive hook) inside an
// HTTP 200, so the status code alone cannot tell a landed push from a refused
// one. This file reads that report out of the response as it streams to the
// client.
//
// When the client asked for side-band, the response is multiplexed: every
// pkt-line's first payload byte is its band. Band 1 carries the report-status
// (itself pkt-line framed), band 2 progress and messages, band 3 a fatal
// error. The remote's message text — GitHub's rule-violation explanation —
// comes from bands 2 and 3.

const (
	// maxReportCapture bounds how much report-status the capture keeps: band 1
	// when the response is multiplexed, the whole body when it is not. A
	// report is one short pkt-line per ref, so 64 KiB is hundreds of refs; a
	// report past it is not read at all, and the push is recorded by its
	// status alone.
	maxReportCapture = 64 << 10

	// remoteMessageLines and remoteMessageMax bound the remote message a
	// push records: its last remoteMessageLines non-progress lines, cut from
	// the front to remoteMessageMax bytes.
	remoteMessageLines = 20
	remoteMessageMax   = 1 << 10

	sideBandData     = 1
	sideBandProgress = 2
	sideBandError    = 3
)

// pushCapabilities is what a push's client asked receive-pack for, as far as
// reading the response goes.
type pushCapabilities struct {
	// sideBand: the response is multiplexed into bands.
	sideBand bool
	// reportStatus: the response carries a per-ref report.
	reportStatus bool
}

// parsePushCapabilities reads the capabilities a receive-pack request
// carries after the NUL on its first command line. Shallow lines may precede
// the commands and carry none, so the first line holding a NUL is the one.
func parsePushCapabilities(block []byte) pushCapabilities {
	var caps pushCapabilities
	for len(block) >= pktLineLenWidth {
		n, ok := pktLineLen(block[:pktLineLenWidth])
		if !ok || n < pktLineLenWidth || n > len(block) {
			break // the flush-pkt ending the commands, or broken framing
		}
		line := block[pktLineLenWidth:n]
		block = block[n:]
		i := bytes.IndexByte(line, 0)
		if i < 0 {
			continue
		}
		for _, c := range strings.Fields(string(line[i+1:])) {
			switch c {
			case "side-band", "side-band-64k":
				caps.sideBand = true
			case "report-status", "report-status-v2":
				caps.reportStatus = true
			}
		}
		break
	}
	return caps
}

// reportCapture keeps what the push's outcome is read from as a receive-pack
// response streams by. It does not know whether the response is multiplexed
// until the request's capabilities are read, which is after the response has
// finished, so it keeps both readings: the raw body for a plain report, and
// the demultiplexed bands for a side-band one. Both are bounded.
type reportCapture struct {
	// raw is the body from its first byte, while it fits maxReportCapture.
	raw     []byte
	rawOver bool

	// pending is the side-band pkt-line being assembled across writes. A
	// pkt-line's length field caps it at 64 KiB.
	pending []byte
	// band1 is the concatenated band-1 data: the report-status.
	band1     []byte
	band1Over bool
	// broken marks a body that does not frame as side-band; done marks its
	// closing flush-pkt. Either ends the demultiplexing.
	broken bool
	done   bool
	// message is bands 2 and 3.
	message messageTail
}

// feed takes the next bytes of the response body.
func (c *reportCapture) feed(b []byte) {
	if !c.rawOver {
		if len(c.raw)+len(b) > maxReportCapture {
			c.raw, c.rawOver = nil, true
		} else {
			c.raw = append(c.raw, b...)
		}
	}
	if c.broken || c.done {
		return
	}
	c.pending = append(c.pending, b...)
	off := 0
	for len(c.pending)-off >= pktLineLenWidth {
		n, ok := pktLineLen(c.pending[off : off+pktLineLenWidth])
		if !ok || (n > 0 && n < pktLineLenWidth) {
			c.broken, c.pending = true, nil
			return
		}
		if n == 0 {
			c.done, c.pending = true, nil
			return
		}
		if len(c.pending)-off < n {
			break // the rest of this pkt-line is in a later write
		}
		c.demux(c.pending[off+pktLineLenWidth : off+n])
		if c.broken {
			c.pending = nil
			return
		}
		off += n
	}
	c.pending = append(c.pending[:0], c.pending[off:]...)
}

// demux routes one side-band pkt-line's payload by its band byte.
func (c *reportCapture) demux(payload []byte) {
	if len(payload) == 0 {
		return
	}
	switch payload[0] {
	case sideBandData:
		if c.band1Over {
			return
		}
		if len(c.band1)+len(payload)-1 > maxReportCapture {
			c.band1, c.band1Over = nil, true
			return
		}
		c.band1 = append(c.band1, payload[1:]...)
	case sideBandProgress, sideBandError:
		c.message.write(payload[1:])
	default:
		c.broken = true // not a band: the body is not multiplexed
	}
}

// result reads the report the captured response carried under the
// capabilities the push asked for, and the remote's message. A nil report
// means none could be read, and cause says why.
func (c *reportCapture) result(caps pushCapabilities) (report *receivePackReport, message, cause string) {
	if caps.sideBand {
		message = c.message.String()
	}
	if !caps.reportStatus {
		return nil, message, "report-status not requested"
	}
	body := c.raw
	if caps.sideBand {
		if c.broken {
			return nil, message, "side-band framing malformed"
		}
		if c.band1Over {
			return nil, message, "report-status over the capture bound"
		}
		body = c.band1
	} else if c.rawOver {
		return nil, message, "report-status over the capture bound"
	}
	if report = parseReportStatus(body); report == nil {
		return nil, message, "report-status truncated or malformed"
	}
	return report, message, ""
}

// receivePackReport is a parsed report-status block.
type receivePackReport struct {
	// unpack is the text after "unpack ": "ok", or the reason the pack was
	// refused, which refuses every ref.
	unpack string
	refs   map[string]refReport
}

// refReport is one ref's line in a report-status block.
type refReport struct {
	ok     bool
	reason string // the "ng" line's reason
}

// refOutcome is the recorded outcome of a ref the push carried.
func (r *receivePackReport) refOutcome(ref string) (rejected bool, reason string) {
	if r.unpack != "ok" {
		return true, "unpack " + r.unpack
	}
	rr, ok := r.refs[ref]
	switch {
	case !ok:
		return true, "not in report-status"
	case rr.ok:
		return false, ""
	default:
		return true, rr.reason
	}
}

// parseReportStatus decodes a report-status block: an "unpack" line, then
// "ok <ref>" and "ng <ref> <reason>" lines, with report-status-v2's
// "option <key> <value>" lines after an "ok", ended by a flush-pkt. It
// returns nil for a block that is truncated (no flush-pkt), malformed, or
// does not open with "unpack".
func parseReportStatus(b []byte) *receivePackReport {
	report := &receivePackReport{refs: map[string]refReport{}}
	sawUnpack := false
	for {
		if len(b) < pktLineLenWidth {
			return nil
		}
		n, ok := pktLineLen(b[:pktLineLenWidth])
		if !ok {
			return nil
		}
		if n == 0 {
			if !sawUnpack {
				return nil
			}
			return report
		}
		if n < pktLineLenWidth || n > len(b) {
			return nil
		}
		line := strings.TrimSuffix(string(b[pktLineLenWidth:n]), "\n")
		b = b[n:]
		if !sawUnpack {
			msg, ok := strings.CutPrefix(line, "unpack ")
			if !ok {
				return nil
			}
			report.unpack, sawUnpack = msg, true
			continue
		}
		kind, rest, _ := strings.Cut(line, " ")
		switch kind {
		case "ok":
			if rest == "" {
				return nil
			}
			report.refs[rest] = refReport{ok: true}
		case "ng":
			ref, reason, _ := strings.Cut(rest, " ")
			if ref == "" {
				return nil
			}
			report.refs[ref] = refReport{reason: reason}
		case "option":
		default:
			return nil
		}
	}
}

// messageTail keeps the end of a push's remote message text as it streams:
// the last remoteMessageLines lines, a line containing a carriage return (a
// progress meter) dropped, each held to its last remoteMessageMax bytes —
// which is all of it the final message can include — plus utf8.UTFMax, so a
// line that was cut still reads as over the bound, and String cuts it where it
// would have cut the whole line.
type messageTail struct {
	lines []string
	cur   []byte
	curCR bool
}

func (m *messageTail) write(p []byte) {
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.extend(p)
			return
		}
		m.extend(p[:i])
		m.endLine()
		p = p[i+1:]
	}
}

func (m *messageTail) extend(p []byte) {
	if m.curCR {
		return
	}
	if bytes.IndexByte(p, '\r') >= 0 {
		m.curCR, m.cur = true, m.cur[:0]
		return
	}
	m.cur = append(m.cur, p...)
	if over := len(m.cur) - remoteMessageMax - utf8.UTFMax; over > 0 {
		m.cur = append(m.cur[:0], m.cur[over:]...)
	}
}

func (m *messageTail) endLine() {
	if !m.curCR {
		m.push(string(m.cur))
	}
	m.cur, m.curCR = m.cur[:0], false
}

func (m *messageTail) push(line string) {
	if len(m.lines) == remoteMessageLines {
		copy(m.lines, m.lines[1:])
		m.lines = m.lines[:remoteMessageLines-1]
	}
	m.lines = append(m.lines, line)
}

// String is the message: the text split on newlines, progress lines dropped,
// the last remoteMessageLines kept, cut from the front to remoteMessageMax
// bytes and trimmed. The text after the last newline counts as a line, as a
// split would have it.
func (m *messageTail) String() string {
	lines := m.lines
	if !m.curCR {
		lines = append(lines[:len(lines):len(lines)], string(m.cur))
	}
	if len(lines) > remoteMessageLines {
		lines = lines[len(lines)-remoteMessageLines:]
	}
	s := strings.Join(lines, "\n")
	if len(s) > remoteMessageMax {
		s = s[len(s)-remoteMessageMax:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
	}
	return strings.TrimSpace(s)
}
