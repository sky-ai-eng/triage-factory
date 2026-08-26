package gitssh

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
)

// pktHeaderLen is the width of a pkt-line's leading hex length field. The four
// hex digits encode the length of the whole line, themselves included.
const pktHeaderLen = 4

// flushPkt ends a request or a response. The other zero-payload control
// packets — delim (0001) and response-end (0002) — appear inside a v2 command
// and its response, so nothing here treats them as boundaries.
var flushPkt = []byte("0000")

// pktReader reads pkt-lines from a stream while preserving their exact bytes,
// so a packet can be relayed onward unchanged after being inspected.
type pktReader struct{ r *bufio.Reader }

func newPktReader(r io.Reader) *pktReader { return &pktReader{r: bufio.NewReader(r)} }

// readPacket returns the next pkt-line's raw bytes (length field included) and
// its payload (length field excluded). A control packet has a nil payload.
// io.EOF at a packet boundary is returned unwrapped, so a caller can treat a
// closed stream as an ordinary end of session.
func (p *pktReader) readPacket() (raw, payload []byte, err error) {
	hdr := make([]byte, pktHeaderLen)
	if _, err := io.ReadFull(p.r, hdr); err != nil {
		if err == io.ErrUnexpectedEOF {
			return nil, nil, fmt.Errorf("truncated pkt-line length field")
		}
		return nil, nil, err
	}
	n, err := strconv.ParseUint(string(hdr), 16, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("malformed pkt-line length %q", hdr)
	}
	if n < pktHeaderLen {
		return hdr, nil, nil
	}
	line := make([]byte, n)
	copy(line, hdr)
	if _, err := io.ReadFull(p.r, line[pktHeaderLen:]); err != nil {
		return nil, nil, fmt.Errorf("truncated pkt-line body: %w", err)
	}
	return line, line[pktHeaderLen:], nil
}

// rest returns whatever is left on the stream after the packets already read.
// The push bridge uses it to stream a packfile — raw bytes, not pkt-lines —
// straight into the request body without buffering it.
func (p *pktReader) rest() io.Reader { return p.r }

// readRawBlock reads pkt-lines up to and including the terminating flush-pkt,
// returning them verbatim. A delim-pkt is part of a v2 command, not a
// terminator, so only a flush ends the block.
func readRawBlock(p *pktReader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		raw, _, err := p.readPacket()
		if err != nil {
			return nil, err
		}
		buf.Write(raw)
		if bytes.Equal(raw, flushPkt) {
			return buf.Bytes(), nil
		}
	}
}

// readRequestBlock is readRawBlock with the empty case named: a block that is
// nothing but its flush carries no request, which is how a v2 client says it
// has no further commands.
func readRequestBlock(p *pktReader) ([]byte, error) {
	block, err := readRawBlock(p)
	if err != nil {
		return nil, err
	}
	if len(block) == pktHeaderLen {
		return nil, nil
	}
	return block, nil
}

// firstAdvertisedPacket returns the first packet of an advertisement that git's
// ssh transport would recognize, consuming the "# service=<name>" packet and
// the flush behind it when smart HTTP put one in front.
//
// Smart HTTP prepends that pair to a v0 advertisement, and git's own
// http-backend omits it under protocol v2 — but the servers we talk to are not
// all git, and a v2 advertisement carrying it anyway is well within what they
// may send. git's ssh transport expects neither, so consuming the pair when it
// is there and nothing when it is not covers both without asking the version.
func firstAdvertisedPacket(p *pktReader) (raw, payload []byte, err error) {
	raw, payload, err = p.readPacket()
	if err != nil {
		return nil, nil, err
	}
	if !bytes.HasPrefix(payload, []byte("# service=")) {
		return raw, payload, nil
	}
	next, _, err := p.readPacket()
	if err != nil {
		return nil, nil, err
	}
	if !bytes.Equal(next, flushPkt) {
		return nil, nil, fmt.Errorf("service advertisement header not followed by a flush-pkt")
	}
	return p.readPacket()
}

// isZeroOID reports whether s is git's all-zeros object id — "no such ref".
// Matched by all-zeros rather than a fixed width so it covers SHA-1 and
// SHA-256 repositories alike.
func isZeroOID(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}
