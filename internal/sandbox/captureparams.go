package sandbox

import (
	"fmt"
	"strings"
)

// CaptureCheckoutsMax bounds how many checkouts one capture names.
const CaptureCheckoutsMax = 64

// CaptureParams names what one capture reads and where it writes.
type CaptureParams struct {
	// Root is the run tree, held by the jail identity that wrote it.
	Root string
	// Checkouts are the checkouts under Root to capture, each as
	// <owner>/<repo>/<slug>, slash-separated.
	Checkouts []string
	// StagingDir is the orchestrator-owned directory whose CaptureStreamFile is
	// the capture's only writable output.
	StagingDir string
}

// ValidateCaptureCheckouts requires each checkout to be exactly three plain
// path segments. The orchestrator names them, the broker puts them in an argv
// and a mount tree it does not own, so a segment that could be an option or a
// traversal is refused here.
func ValidateCaptureCheckouts(rels []string) error {
	if len(rels) > CaptureCheckoutsMax {
		return fmt.Errorf("sandbox: capture names %d checkouts, at most %d", len(rels), CaptureCheckoutsMax)
	}
	seen := make(map[string]bool, len(rels))
	for _, rel := range rels {
		segs := strings.Split(rel, "/")
		if len(segs) != 3 {
			return fmt.Errorf("sandbox: capture checkout %q is not <owner>/<repo>/<slug>", rel)
		}
		for _, seg := range segs {
			if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, "-") || strings.ContainsAny(seg, "\\\x00") {
				return fmt.Errorf("sandbox: capture checkout %q has an unusable segment %q", rel, seg)
			}
		}
		if seen[rel] {
			return fmt.Errorf("sandbox: capture checkout %q is named twice", rel)
		}
		seen[rel] = true
	}
	return nil
}
