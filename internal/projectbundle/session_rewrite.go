package projectbundle

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

type byteReplacement struct {
	old []byte
	new []byte
}

func buildSessionReplacements(oldSessionID, newSessionID, oldCwd, newCwd string) []byteReplacement {
	out := make([]byteReplacement, 0, 2)
	if oldSessionID != "" && oldSessionID != newSessionID {
		out = append(out, byteReplacement{
			old: []byte(oldSessionID),
			new: []byte(newSessionID),
		})
	}
	if oldCwd != "" && oldCwd != newCwd {
		out = append(out, byteReplacement{
			old: []byte(oldCwd),
			new: []byte(newCwd),
		})
	}
	return out
}

// rewriteByLine performs byte-substring replacement on each line from src and
// writes to dst. The "per-line" contract matches the bundle format's decision
// record and avoids scanner token limits by using ReadBytes('\n').
func rewriteByLine(dst io.Writer, src io.Reader, reps []byteReplacement) error {
	br := bufio.NewReader(src)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			line = applyReplacements(line, reps)
			if _, wErr := dst.Write(line); wErr != nil {
				return wErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func applyReplacements(in []byte, reps []byteReplacement) []byte {
	out := in
	for _, rep := range reps {
		if len(rep.old) == 0 {
			continue
		}
		out = bytes.ReplaceAll(out, rep.old, rep.new)
	}
	return out
}

func rewriteToFile(dst io.Writer, src io.Reader, reps []byteReplacement) error {
	if err := rewriteByLine(dst, src, reps); err != nil {
		return fmt.Errorf("rewrite session artifact: %w", err)
	}
	return nil
}
