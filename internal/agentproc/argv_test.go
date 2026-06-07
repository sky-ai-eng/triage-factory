package agentproc

import (
	"slices"
	"testing"
)

func TestBuildArgs_InitialInvocation(t *testing.T) {
	got := BuildArgs(RunOptions{
		Message:      "do the thing",
		Model:        "sonnet-4-6",
		AllowedTools: "Read,Write",
		MaxTurns:     100,
	})
	want := []string{
		"-p", "do the thing",
		"--model", "sonnet-4-6",
		"--output-format", "stream-json",
		"--verbose",
		"--allowedTools", "Read,Write",
		"--max-turns", "100",
	}
	if !slices.Equal(got, want) {
		t.Errorf("argv mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_Interactive(t *testing.T) {
	got := BuildArgs(RunOptions{
		Interactive:  true,
		Message:      "this is sent over stdin, not argv",
		Model:        "sonnet-4-6",
		AllowedTools: "Read,Write",
		MaxTurns:     50,
	})

	// Streaming-input mode must request the stream-json input format and
	// must NOT carry the one-shot -p prompt (the initial message goes
	// over stdin once the wrapper signals ready).
	if slices.Contains(got, "-p") {
		t.Errorf("interactive argv must omit -p: %v", got)
	}
	if slices.Contains(got, "this is sent over stdin, not argv") {
		t.Errorf("interactive argv must omit Message: %v", got)
	}
	ifIdx := slices.Index(got, "--input-format")
	if ifIdx < 0 {
		t.Fatalf("expected --input-format flag: %v", got)
	}
	if got[ifIdx+1] != "stream-json" {
		t.Errorf("--input-format value = %q, want stream-json", got[ifIdx+1])
	}

	// The shared flags still flow through unchanged.
	if !slices.Contains(got, "--output-format") || !slices.Contains(got, "--verbose") {
		t.Errorf("missing fixed flags: %v", got)
	}
	if !slices.Contains(got, "--allowedTools") || !slices.Contains(got, "--max-turns") {
		t.Errorf("expected allowedTools + max-turns to flow through: %v", got)
	}
}

func TestBuildArgs_ResumeAddsResumeFlagBeforeModel(t *testing.T) {
	// --resume must precede --model so claude treats this as a
	// continuation of the captured session, not a fresh run that
	// happens to share a model. Order matters to claude's flag
	// parsing in -p mode.
	got := BuildArgs(RunOptions{
		Message:      "answer to your question",
		SessionID:    "sess-123",
		Model:        "sonnet-4-6",
		AllowedTools: "Read",
	})
	resumeIdx := slices.Index(got, "--resume")
	modelIdx := slices.Index(got, "--model")
	if resumeIdx < 0 || modelIdx < 0 {
		t.Fatalf("missing flags: %v", got)
	}
	if resumeIdx > modelIdx {
		t.Errorf("--resume must come before --model: %v", got)
	}
	if got[resumeIdx+1] != "sess-123" {
		t.Errorf("resume id = %q, want sess-123", got[resumeIdx+1])
	}
}

func TestBuildArgs_OmitsZeroValueFlags(t *testing.T) {
	got := BuildArgs(RunOptions{Message: "hi"})
	for _, flag := range []string{"--model", "--resume", "--allowedTools", "--max-turns", "--add-dir"} {
		if slices.Contains(got, flag) {
			t.Errorf("expected %q to be omitted, got %v", flag, got)
		}
	}
	if !slices.Contains(got, "--output-format") || !slices.Contains(got, "--verbose") {
		t.Errorf("missing fixed flags: %v", got)
	}
}

func TestBuildArgs_AddDirsEmitsOnePerEntry(t *testing.T) {
	// --add-dir is repeatable; CC's path-scoped tool checks (notably
	// the rm guard) treat each entry as an independent allowed dir
	// rather than parsing a comma-joined list. Empty strings are
	// dropped so callers can pass a slice with conditional entries
	// without filtering at the call site.
	got := BuildArgs(RunOptions{
		Message: "hi",
		AddDirs: []string{"/a", "", "/b/c"},
	})
	flags := []int{}
	for i, v := range got {
		if v == "--add-dir" {
			flags = append(flags, i)
		}
	}
	if len(flags) != 2 {
		t.Fatalf("expected 2 --add-dir entries, got %d: %v", len(flags), got)
	}
	if got[flags[0]+1] != "/a" || got[flags[1]+1] != "/b/c" {
		t.Errorf("add-dir values wrong: %v", got)
	}
}
