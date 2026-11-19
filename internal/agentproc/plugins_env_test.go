package agentproc

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

const testAllowPrefix = "CLAUDE_LOOP_HARNESS_"

// writeSDKFile writes content at rel under dir, creating parents.
func writeSDKFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	sdkScriptRel = "node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs"
	sdkBinaryRel = "node_modules/@anthropic-ai/claude-agent-sdk-linux-x64/claude"
)

// fakeSDKTree writes the two files a scan needs: the SDK's script and the
// platform binary. A binary's text is mostly bytes no pattern matches, so it
// carries the key between NULs.
func fakeSDKTree(t *testing.T, scriptText, binaryText string) string {
	t.Helper()
	dir := t.TempDir()
	writeSDKFile(t, dir, sdkScriptRel, scriptText)
	writeSDKFile(t, dir, sdkBinaryRel, "\x7fELF\x00"+binaryText+"\x00\x01")
	return dir
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestPluginEnvKeyAllowed_OperatorPrefix(t *testing.T) {
	key := testAllowPrefix + "MODE"

	t.Run("refused before verification", func(t *testing.T) {
		isolatePluginEnv(t)
		t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
		ok, reason := PluginEnvKeyAllowed(key)
		if ok {
			t.Fatal("allowed before the SDK was scanned")
		}
		if !strings.Contains(reason, "not been checked") || !strings.Contains(reason, testAllowPrefix) {
			t.Errorf("reason = %q, want it to say the prefix has not been checked and name it", reason)
		}
	})

	t.Run("allowed after a clean scan", func(t *testing.T) {
		isolatePluginEnv(t)
		t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
		sdk := fakeSDKTree(t, `env.CLAUDE_CODE_FOO; env.CLAUDE_CONFIG_DIR`, "CLAUDE_OTHER_THING")
		if err := VerifyPluginEnvAllowPrefixes(sdk); err != nil {
			t.Fatalf("VerifyPluginEnvAllowPrefixes: %v", err)
		}
		if ok, reason := PluginEnvKeyAllowed(key); !ok {
			t.Errorf("refused after a clean scan: %s", reason)
		}
		// Only the allowed prefix opens, not CLAUDE_ as a whole.
		if ok, reason := PluginEnvKeyAllowed("CLAUDE_CODE_FOO"); ok || !strings.Contains(reason, "reserved for Claude Code") {
			t.Errorf("CLAUDE_CODE_FOO = %v, %q; want refused as reserved", ok, reason)
		}
		if ok, _ := PluginEnvKeyAllowed("CLAUDE_LOOP_X"); ok {
			t.Error("a key outside the allowed prefix was allowed")
		}
	})

	t.Run("refused when the SDK reads a key under the prefix", func(t *testing.T) {
		isolatePluginEnv(t)
		t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
		sdk := fakeSDKTree(t, `x`, "CLAUDE_LOOP_HARNESS_SOMETHING")
		err := VerifyPluginEnvAllowPrefixes(sdk)
		if err == nil {
			t.Fatal("VerifyPluginEnvAllowPrefixes accepted a prefix the SDK reads a key under")
		}
		for _, want := range []string{testAllowPrefix, "CLAUDE_LOOP_HARNESS_SOMETHING"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %s", err, want)
			}
		}
		if ok, reason := PluginEnvKeyAllowed(key); ok || !strings.Contains(reason, "not been checked") {
			t.Errorf("PluginEnvKeyAllowed = %v, %q; want refused", ok, reason)
		}
	})

	t.Run("a key in the SDK's script counts too", func(t *testing.T) {
		isolatePluginEnv(t)
		t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
		sdk := fakeSDKTree(t, `process.env.CLAUDE_LOOP_HARNESS_SECRET_THING`, "nothing")
		if err := VerifyPluginEnvAllowPrefixes(sdk); err == nil {
			t.Fatal("a key in the SDK's JavaScript did not refuse the prefix")
		}
	})

	t.Run("refused again when the configured value changes", func(t *testing.T) {
		isolatePluginEnv(t)
		t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
		sdk := fakeSDKTree(t, `x`, "nothing")
		if err := VerifyPluginEnvAllowPrefixes(sdk); err != nil {
			t.Fatalf("VerifyPluginEnvAllowPrefixes: %v", err)
		}
		if ok, _ := PluginEnvKeyAllowed(key); !ok {
			t.Fatal("not allowed after verification")
		}
		t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix+",CLAUDE_OTHER_PLUGIN_")
		ok, reason := PluginEnvKeyAllowed(key)
		if ok || !strings.Contains(reason, "not been checked") {
			t.Errorf("after the value changed: %v, %q; want refused until scanned again", ok, reason)
		}
		// Scanned again, the new value is honored.
		if err := VerifyPluginEnvAllowPrefixes(sdk); err != nil {
			t.Fatalf("second VerifyPluginEnvAllowPrefixes: %v", err)
		}
		for _, k := range []string{key, "CLAUDE_OTHER_PLUGIN_X"} {
			if ok, reason := PluginEnvKeyAllowed(k); !ok {
				t.Errorf("%s refused after the second scan: %s", k, reason)
			}
		}
	})

	t.Run("a credential-shaped key under an allowed prefix is still refused", func(t *testing.T) {
		isolatePluginEnv(t)
		t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
		if err := VerifyPluginEnvAllowPrefixes(fakeSDKTree(t, `x`, "nothing")); err != nil {
			t.Fatalf("VerifyPluginEnvAllowPrefixes: %v", err)
		}
		for _, k := range []string{
			testAllowPrefix + "TOKEN",
			testAllowPrefix + "API_KEY",
			testAllowPrefix + "SECRET",
			testAllowPrefix + "PASSWORD",
			testAllowPrefix + "CREDENTIAL",
			testAllowPrefix + "AUTH",
		} {
			if ok, _ := PluginEnvKeyAllowed(k); ok {
				t.Errorf("%s allowed", k)
			}
		}
		ok, reason := PluginEnvKeyAllowed(testAllowPrefix + "TOKEN")
		if ok || !strings.Contains(reason, "TOKEN") {
			t.Errorf("reason = %q, want it to name TOKEN", reason)
		}
	})

	t.Run("without a configured prefix the reason names the setting", func(t *testing.T) {
		isolatePluginEnv(t)
		ok, reason := PluginEnvKeyAllowed(key)
		if ok || !strings.Contains(reason, envPluginEnvAllowPrefixes) {
			t.Errorf("= %v, %q; want a refusal naming %s", ok, reason, envPluginEnvAllowPrefixes)
		}
	})
}

func TestPluginEnvKeyAllowed_PrefixThroughValidate(t *testing.T) {
	isolatePluginEnv(t)
	t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
	list := []domain.LocalPlugin{{
		Path: newPluginDir(t),
		Env:  map[string]string{testAllowPrefix + "MODE": "1"},
	}}
	faults := ValidateLocalPlugins(list)
	if len(faults) != 1 {
		t.Fatalf("before verification: faults = %v, want one", faults)
	}
	requireFault(t, faults, "local_plugins[0].env."+testAllowPrefix+"MODE", "not been checked")
	if err := VerifyPluginEnvAllowPrefixes(fakeSDKTree(t, `x`, "nothing")); err != nil {
		t.Fatal(err)
	}
	if faults := ValidateLocalPlugins(list); len(faults) != 0 {
		t.Errorf("after verification: faults = %v, want none", faults)
	}
}

func TestParsePluginEnvAllowPrefixes(t *testing.T) {
	good, err := ParsePluginEnvAllowPrefixes("CLAUDE_LOOP_HARNESS_")
	if err != nil || !slices.Equal(good, []string{"CLAUDE_LOOP_HARNESS_"}) {
		t.Errorf("CLAUDE_LOOP_HARNESS_ = %v, %v; want accepted", good, err)
	}

	for _, bad := range []string{
		"CLAUDE_X_",           // one segment
		"CLAUDE_",             // no segments
		"LOOP_HARNESS_",       // no CLAUDE_
		"CLAUDE_LOOP_HARNESS", // no trailing underscore
		"CLAUDE__X_",          // empty segment
		"CLAUDE_X__Y_",        // empty segment between two
		"CLAUDE_loop_harness_",
		"CLAUDE_LOOP-HARNESS_",
		"claude_loop_harness_",
	} {
		got, err := ParsePluginEnvAllowPrefixes(bad)
		if err == nil {
			t.Errorf("%q accepted as %v", bad, got)
		}
		if len(got) != 0 {
			t.Errorf("%q returned prefixes %v alongside its error", bad, got)
		}
	}
}

func TestParsePluginEnvAllowPrefixes_List(t *testing.T) {
	got, err := ParsePluginEnvAllowPrefixes(" CLAUDE_LOOP_HARNESS_ , ,CLAUDE_A1_B2_,CLAUDE_LOOP_HARNESS_ ")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"CLAUDE_LOOP_HARNESS_", "CLAUDE_A1_B2_"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v (trimmed, blanks skipped, duplicates collapsed)", got, want)
	}
	if got, err := ParsePluginEnvAllowPrefixes(""); len(got) != 0 || err != nil {
		t.Errorf("empty = %v, %v; want nothing and no error", got, err)
	}

	// One bad member does not discard the good ones, and every bad one is
	// reported.
	got, err = ParsePluginEnvAllowPrefixes("CLAUDE_LOOP_HARNESS_,CLAUDE_X_,LOOP_")
	if !slices.Equal(got, []string{"CLAUDE_LOOP_HARNESS_"}) {
		t.Errorf("got %v, want the good prefix kept", got)
	}
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_X_") || !strings.Contains(err.Error(), "LOOP_") {
		t.Errorf("err = %v, want both bad prefixes named", err)
	}
}

func TestScanSDKClaudeKeys(t *testing.T) {
	t.Run("finds keys from the script and the binary", func(t *testing.T) {
		sdk := fakeSDKTree(t, `const a = process.env.CLAUDE_SDK_KEY_A || "x"; CLAUDE_CODE_B`, "junk\x00CLAUDE_BIN_KEY_B\x01")
		got, err := ScanSDKClaudeKeys(sdk)
		if err != nil {
			t.Fatalf("ScanSDKClaudeKeys: %v", err)
		}
		want := []string{"CLAUDE_BIN_KEY_B", "CLAUDE_CODE_B", "CLAUDE_SDK_KEY_A"}
		if !slices.Equal(keysOf(got), want) {
			t.Errorf("keys = %v, want %v", keysOf(got), want)
		}
	})

	t.Run("reads every script suffix and the Windows binary name", func(t *testing.T) {
		dir := t.TempDir()
		writeSDKFile(t, dir, "node_modules/@anthropic-ai/claude-agent-sdk/a.mjs", "CLAUDE_FROM_MJS")
		writeSDKFile(t, dir, "node_modules/@anthropic-ai/claude-agent-sdk/b.js", "CLAUDE_FROM_JS")
		writeSDKFile(t, dir, "node_modules/@anthropic-ai/claude-agent-sdk/c.cjs", "CLAUDE_FROM_CJS")
		writeSDKFile(t, dir, "node_modules/@anthropic-ai/claude-agent-sdk/readme.md", "CLAUDE_FROM_MARKDOWN")
		writeSDKFile(t, dir, "node_modules/@anthropic-ai/claude-agent-sdk-win32-x64/claude.exe", "CLAUDE_FROM_EXE")
		got, err := ScanSDKClaudeKeys(dir)
		if err != nil {
			t.Fatalf("ScanSDKClaudeKeys: %v", err)
		}
		want := []string{"CLAUDE_FROM_CJS", "CLAUDE_FROM_EXE", "CLAUDE_FROM_JS", "CLAUDE_FROM_MJS"}
		if !slices.Equal(keysOf(got), want) {
			t.Errorf("keys = %v, want %v", keysOf(got), want)
		}
	})

	t.Run("skips a nested node_modules and other packages", func(t *testing.T) {
		sdk := fakeSDKTree(t, "CLAUDE_TOP", "CLAUDE_BIN")
		writeSDKFile(t, sdk, "node_modules/@anthropic-ai/claude-agent-sdk/node_modules/dep/index.js", "CLAUDE_NESTED_DEP")
		writeSDKFile(t, sdk, "node_modules/@anthropic-ai/other-package/index.mjs", "CLAUDE_OTHER_PACKAGE")
		got, err := ScanSDKClaudeKeys(sdk)
		if err != nil {
			t.Fatalf("ScanSDKClaudeKeys: %v", err)
		}
		if !slices.Equal(keysOf(got), []string{"CLAUDE_BIN", "CLAUDE_TOP"}) {
			t.Errorf("keys = %v", keysOf(got))
		}
	})

	t.Run("no binary is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeSDKFile(t, dir, sdkScriptRel, "CLAUDE_X")
		_, err := ScanSDKClaudeKeys(dir)
		if err == nil || !strings.Contains(err.Error(), "no Claude Code binary") {
			t.Errorf("err = %v, want a missing-binary error", err)
		}
	})

	t.Run("no scripts is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeSDKFile(t, dir, sdkBinaryRel, "CLAUDE_X")
		_, err := ScanSDKClaudeKeys(dir)
		if err == nil || !strings.Contains(err.Error(), "no agent SDK scripts") {
			t.Errorf("err = %v, want a missing-scripts error", err)
		}
	})

	t.Run("a package dir with neither is an error", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "node_modules", "@anthropic-ai", "claude-agent-sdk"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := ScanSDKClaudeKeys(dir); err == nil {
			t.Error("an empty SDK package scanned clean")
		}
	})

	t.Run("no @anthropic-ai directory is an error", func(t *testing.T) {
		if _, err := ScanSDKClaudeKeys(t.TempDir()); err == nil {
			t.Error("a directory with no SDK scanned clean")
		}
		if _, err := ScanSDKClaudeKeys(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Error("a missing directory scanned clean")
		}
	})
}

func TestScanFileForClaudeKeys_KeyStraddlesTheChunkBoundary(t *testing.T) {
	const (
		chunk = 1 << 20
		key   = "CLAUDE_STRADDLE_BOUNDARY_KEY"
	)
	// start is where the key begins; the first chunk ends at offset chunk.
	for name, start := range map[string]int{
		"split mid-key":            chunk - 10,
		"ends exactly at the edge": chunk - len(key),
		"last byte in next chunk":  chunk - len(key) + 1,
		"first byte before edge":   chunk - 1,
		"begins at the edge":       chunk,
	} {
		t.Run(name, func(t *testing.T) {
			// Spaces never match the key pattern, so the file holds exactly one.
			content := []byte(strings.Repeat(" ", chunk+4096))
			copy(content[start:], key)
			path := filepath.Join(t.TempDir(), "big.bin")
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			keys := map[string]struct{}{}
			if err := scanFileForClaudeKeys(path, keys); err != nil {
				t.Fatalf("scanFileForClaudeKeys: %v", err)
			}
			if !slices.Equal(keysOf(keys), []string{key}) {
				t.Errorf("keys = %v, want exactly [%s] (a truncated prefix must not be recorded)", keysOf(keys), key)
			}
		})
	}
}

func TestScanFileForClaudeKeys_KeysAcrossAMultiChunkFile(t *testing.T) {
	const chunk = 1 << 20
	content := []byte(strings.Repeat(" ", 3*chunk+100))
	places := map[string]int{
		"CLAUDE_AT_START":    0,
		"CLAUDE_MID_FIRST":   chunk/2 + 7,
		"CLAUDE_SECOND_EDGE": 2*chunk - 5,
		"CLAUDE_AT_END":      len(content) - len("CLAUDE_AT_END"),
	}
	for k, at := range places {
		copy(content[at:], k)
	}
	path := filepath.Join(t.TempDir(), "multi.bin")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	keys := map[string]struct{}{}
	if err := scanFileForClaudeKeys(path, keys); err != nil {
		t.Fatalf("scanFileForClaudeKeys: %v", err)
	}
	want := make([]string, 0, len(places))
	for k := range places {
		want = append(want, k)
	}
	sort.Strings(want)
	if !slices.Equal(keysOf(keys), want) {
		t.Errorf("keys = %v, want %v", keysOf(keys), want)
	}
}

func TestScanFileForClaudeKeys_MissingFileIsAnError(t *testing.T) {
	if err := scanFileForClaudeKeys(filepath.Join(t.TempDir(), "absent"), map[string]struct{}{}); err == nil {
		t.Error("a missing file scanned clean")
	}
}

func TestVerifyPluginEnvAllowPrefixes_ScanErrorLeavesThePrefixRefused(t *testing.T) {
	isolatePluginEnv(t)
	t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
	key := testAllowPrefix + "MODE"

	empty := t.TempDir()
	err := VerifyPluginEnvAllowPrefixes(empty)
	if err == nil {
		t.Fatal("a scan that could not run returned nil")
	}
	if !strings.Contains(err.Error(), "no prefix is honored") {
		t.Errorf("err = %v", err)
	}
	if ok, reason := PluginEnvKeyAllowed(key); ok || !strings.Contains(reason, "not been checked") {
		t.Fatalf("after a failed scan: %v, %q; want the key refused", ok, reason)
	}

	// An incomplete install is a failed scan too.
	partial := t.TempDir()
	writeSDKFile(t, partial, sdkScriptRel, "x")
	if err := VerifyPluginEnvAllowPrefixes(partial); err == nil {
		t.Fatal("an SDK with no binary verified clean")
	}
	if ok, _ := PluginEnvKeyAllowed(key); ok {
		t.Fatal("key allowed after a scan that found no binary")
	}

	// The failure was not remembered as a pass: the next call scans again.
	if err := VerifyPluginEnvAllowPrefixes(fakeSDKTree(t, "x", "y")); err != nil {
		t.Fatalf("retry against a complete SDK: %v", err)
	}
	if ok, reason := PluginEnvKeyAllowed(key); !ok {
		t.Errorf("refused after a good scan: %s", reason)
	}
}

func TestVerifyPluginEnvAllowPrefixes_AlreadyVerifiedValueIsNotScannedAgain(t *testing.T) {
	isolatePluginEnv(t)
	t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix)
	if err := VerifyPluginEnvAllowPrefixes(fakeSDKTree(t, "x", "y")); err != nil {
		t.Fatal(err)
	}
	// A directory that would fail a scan returns at once for a verified value.
	if err := VerifyPluginEnvAllowPrefixes(t.TempDir()); err != nil {
		t.Errorf("second call = %v, want nil without a scan", err)
	}
	if ok, _ := PluginEnvKeyAllowed(testAllowPrefix + "MODE"); !ok {
		t.Error("verified prefix lost")
	}
}

func TestVerifyPluginEnvAllowPrefixes_NoPrefixNeedsNoScan(t *testing.T) {
	isolatePluginEnv(t)
	if err := VerifyPluginEnvAllowPrefixes(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Errorf("no prefixes configured: err = %v, want nil", err)
	}
}

func TestVerifyPluginEnvAllowPrefixes_MalformedPrefixIsReportedAndNotHonored(t *testing.T) {
	isolatePluginEnv(t)
	t.Setenv(envPluginEnvAllowPrefixes, "CLAUDE_X_")
	err := VerifyPluginEnvAllowPrefixes(fakeSDKTree(t, "x", "y"))
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_X_") {
		t.Errorf("err = %v, want the malformed prefix named", err)
	}
	if ok, _ := PluginEnvKeyAllowed("CLAUDE_X_FOO"); ok {
		t.Error("a malformed prefix opened a key")
	}
}

func TestVerifyPluginEnvAllowPrefixes_ClearsOneAndRefusesAnother(t *testing.T) {
	isolatePluginEnv(t)
	t.Setenv(envPluginEnvAllowPrefixes, testAllowPrefix+",CLAUDE_SDK_OWNED_")
	sdk := fakeSDKTree(t, "env.CLAUDE_SDK_OWNED_FLAG", "nothing")
	err := VerifyPluginEnvAllowPrefixes(sdk)
	if err == nil || !strings.Contains(err.Error(), "CLAUDE_SDK_OWNED_FLAG") {
		t.Fatalf("err = %v, want the SDK's key named", err)
	}
	if ok, reason := PluginEnvKeyAllowed(testAllowPrefix + "MODE"); !ok {
		t.Errorf("the clean prefix was refused with the other: %s", reason)
	}
	if ok, _ := PluginEnvKeyAllowed("CLAUDE_SDK_OWNED_X"); ok {
		t.Error("the prefix the SDK reads a key under was honored")
	}
}

// TestScanFileForClaudeKeys_FindsAKeyAtAnyOffsetNearTheChunkEdges places one
// key at each offset across the stretches around the read boundary where a
// split key could be lost, and requires the whole key every time: across the
// boundary itself, and around where a fixed-width carry would begin.
func TestScanFileForClaudeKeys_FindsAKeyAtAnyOffsetNearTheChunkEdges(t *testing.T) {
	const chunk = 1 << 20
	const key = "CLAUDE_EDGE_PROBE_KEY"
	var offsets []int
	for o := chunk - 530; o <= chunk-490; o += 4 {
		offsets = append(offsets, o)
	}
	for o := chunk - len(key) - 2; o <= chunk+2; o++ {
		offsets = append(offsets, o)
	}
	dir := t.TempDir()
	for _, off := range offsets {
		body := make([]byte, chunk+4096)
		for i := range body {
			body[i] = '.'
		}
		copy(body[off:], key)
		path := filepath.Join(dir, "f.bin")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		keys := map[string]struct{}{}
		if err := scanFileForClaudeKeys(path, keys); err != nil {
			t.Fatal(err)
		}
		if _, ok := keys[key]; !ok || len(keys) != 1 {
			t.Errorf("key at offset %d: found %v, want exactly %s", off, keys, key)
		}
	}
}
