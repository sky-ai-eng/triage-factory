#!/usr/bin/env bun
/**
 * Differential parity harness: runs pi's actual TypeScript tools (imported
 * from the local pi checkout) and the Rust `tf-harness-tools` CLI against identical
 * fixture workspaces, then deep-compares the result envelopes.
 *
 * Usage:
 *   bun harness/parity/parity.ts [--pi /path/to/pi] [--cli /path/to/tf-harness-tools] [case-name-filter]
 *
 * Requires: `npm ci` in the pi checkout, `cargo build` in harness/.
 *
 * Normalizations before comparison (each one is a place where byte equality
 * is impossible or meaningless, not a semantic difference):
 *   - the per-case workspace directory path -> "$DIR"
 *   - /tmp/pi-bash-<hex>.log paths -> "$TMPFILE" (contents are compared)
 *   - image `data` payloads -> "<image>" (encoders differ; mime + note text
 *     still compared)
 */

import { mkdirSync, readFileSync, rmSync, writeFileSync, symlinkSync, chmodSync, existsSync } from "node:fs";
import { join, resolve } from "node:path";

const argv = process.argv.slice(2);
let piRoot = process.env.PI_ROOT ?? resolve(import.meta.dir, "../../../pi");
let cliPath = resolve(import.meta.dir, "../target/debug/tf-harness-tools");
let filter: string | undefined;
for (let i = 0; i < argv.length; i++) {
  if (argv[i] === "--pi") piRoot = argv[++i];
  else if (argv[i] === "--cli") cliPath = argv[++i];
  else filter = argv[i];
}

const toolsModule = await import(join(piRoot, "packages/coding-agent/src/core/tools/index.ts"));

type Envelope = {
  ok: boolean;
  result?: unknown;
  error?: string;
  tmpfiles?: Record<string, string>;
  postFiles?: Record<string, string | null>;
};

interface Case {
  name: string;
  tool: "read" | "bash" | "edit" | "write" | "grep" | "find" | "ls";
  setup?: (dir: string) => void;
  args: (dir: string) => Record<string, unknown>;
  /** Relative paths whose post-run contents are part of the comparison. */
  postFiles?: string[];
  /**
   * Sort result body lines before comparing: rg/fd walk in parallel on the
   * pi side, so multi-file match ordering is nondeterministic there. The
   * `\n\n[...]` notices footer is compared in place.
   */
  sortLines?: boolean;
  /** Skip reason, when a case documents a known intentional divergence. */
  skip?: string;
}

function sortBodyLines(env: Envelope): void {
  const result = env.result as { content?: Array<{ type: string; text?: string }> } | undefined;
  if (!result?.content) return;
  for (const block of result.content) {
    if (block.type !== "text" || typeof block.text !== "string") continue;
    const footerStart = block.text.indexOf("\n\n[");
    const body = footerStart === -1 ? block.text : block.text.slice(0, footerStart);
    const footer = footerStart === -1 ? "" : block.text.slice(footerStart);
    block.text = body.split("\n").sort().join("\n") + footer;
  }
}

function normalizeEnvelope(env: Envelope, dir: string): Envelope {
  let text = JSON.stringify(env);
  // Workspace dir -> $DIR (JSON-escaped form is identical for plain paths).
  text = text.replaceAll(dir, "$DIR");
  const clone = JSON.parse(text) as Envelope;

  // Branding divergence: strip fd's stderr framing pi passes through; the
  // Rust side emits the bare globset message.
  if (typeof clone.error === "string" && clone.error.startsWith("[fd error]: ")) {
    clone.error = clone.error.slice("[fd error]: ".length);
  }

  // Collect + normalize bash temp files (pi-bash upstream, tf-bash here —
  // a deliberate branding divergence).
  const tmpfiles: Record<string, string> = {};
  let tmpIndex = 0;
  let text2 = JSON.stringify(clone);
  text2 = text2.replace(/\/tmp\/(?:pi|tf)-bash-[0-9a-f]{16}\.log/g, (m) => {
    const key = `$TMPFILE${tmpIndex}`;
    try {
      tmpfiles[key] = readFileSync(m, "utf-8");
    } catch {
      tmpfiles[key] = "<unreadable>";
    }
    tmpIndex++;
    return key;
  });
  const out = JSON.parse(text2) as Envelope;
  if (Object.keys(tmpfiles).length > 0) out.tmpfiles = tmpfiles;

  // Blank image payloads.
  const result = out.result as { content?: Array<{ type: string; data?: string }> } | undefined;
  if (result?.content) {
    for (const block of result.content) {
      if (block.type === "image" && typeof block.data === "string") block.data = "<image>";
    }
  }
  return out;
}

function collectPostFiles(env: Envelope, dir: string, postFiles?: string[]): void {
  if (!postFiles) return;
  env.postFiles = {};
  for (const rel of postFiles) {
    try {
      env.postFiles[rel] = Buffer.from(readFileSync(join(dir, rel))).toString("base64");
    } catch {
      env.postFiles[rel] = null;
    }
  }
}

async function runPi(c: Case, dir: string): Promise<Envelope> {
  const tool = toolsModule.createTool(c.tool, dir);
  let env: Envelope;
  try {
    // The agent runtime applies prepareArguments before execute; mirror it.
    const rawArgs = c.args(dir);
    const prepared = tool.prepareArguments ? tool.prepareArguments(rawArgs) : rawArgs;
    const raw = await tool.execute(`parity-${c.name}`, prepared);
    env = { ok: true, result: { content: raw.content, ...(raw.details !== undefined ? { details: raw.details } : {}) } };
  } catch (err) {
    env = { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
  collectPostFiles(env, dir, c.postFiles);
  return normalizeEnvelope(env, dir);
}

async function runRust(c: Case, dir: string): Promise<Envelope> {
  const proc = Bun.spawn([cliPath, c.tool, "--cwd", dir, JSON.stringify(c.args(dir))], {
    stdout: "pipe",
    stderr: "pipe",
  });
  const stdout = await new Response(proc.stdout).text();
  await proc.exited;
  let env: Envelope;
  try {
    const parsed = JSON.parse(stdout.trim());
    env = parsed.ok ? { ok: true, result: parsed.result } : { ok: false, error: parsed.error };
  } catch {
    env = { ok: false, error: `<CLI produced unparseable output: ${stdout.slice(0, 200)}>` };
  }
  collectPostFiles(env, dir, c.postFiles);
  return normalizeEnvelope(env, dir);
}

function deepDiff(a: unknown, b: unknown, path = "$"): string[] {
  if (typeof a !== typeof b) return [`${path}: type ${typeof a} vs ${typeof b}`];
  if (a === null || b === null || typeof a !== "object") {
    if (a !== b) {
      const fmt = (v: unknown) => JSON.stringify(v);
      return [`${path}: ${fmt(a)} vs ${fmt(b)}`];
    }
    return [];
  }
  if (Array.isArray(a) !== Array.isArray(b)) return [`${path}: array-ness differs`];
  const ao = a as Record<string, unknown>;
  const bo = b as Record<string, unknown>;
  const keys = new Set([...Object.keys(ao), ...Object.keys(bo)]);
  const diffs: string[] = [];
  for (const key of keys) {
    if (!(key in ao)) diffs.push(`${path}.${key}: missing on pi side`);
    else if (!(key in bo)) diffs.push(`${path}.${key}: missing on rust side`);
    else diffs.push(...deepDiff(ao[key], bo[key], `${path}.${key}`));
  }
  return diffs;
}

// ---------------------------------------------------------------- fixtures

function file(dir: string, rel: string, content: string | Buffer): void {
  const p = join(dir, rel);
  mkdirSync(resolve(p, ".."), { recursive: true });
  writeFileSync(p, content);
}

const png1x1 = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg==",
  "base64",
);

function tinyBmp(): Buffer {
  const b = Buffer.alloc(58);
  b.write("BM", 0, "ascii");
  b.writeUInt32LE(b.length, 2);
  b.writeUInt32LE(54, 10);
  b.writeUInt32LE(40, 14);
  b.writeInt32LE(1, 18);
  b.writeInt32LE(1, 22);
  b.writeUInt16LE(1, 26);
  b.writeUInt16LE(24, 28);
  b.writeUInt32LE(0, 30);
  b.writeUInt32LE(4, 34);
  b[56] = 0xff;
  return b;
}

const seq = (n: number, f: (i: number) => string) => Array.from({ length: n }, (_, i) => f(i + 1));

const cases: Case[] = [
  // ------------------------------------------------------------------ read
  {
    name: "read-simple",
    tool: "read",
    setup: (d) => file(d, "a.txt", "Hello, world!\nLine 2\nLine 3"),
    args: () => ({ path: "a.txt" }),
  },
  {
    name: "read-trailing-newline",
    tool: "read",
    setup: (d) => file(d, "a.txt", "one\ntwo\n"),
    args: () => ({ path: "a.txt" }),
  },
  {
    name: "read-empty-file",
    tool: "read",
    setup: (d) => file(d, "empty.txt", ""),
    args: () => ({ path: "empty.txt" }),
  },
  {
    name: "read-line-truncation",
    tool: "read",
    setup: (d) => file(d, "big.txt", seq(2500, (i) => `Line ${i}`).join("\n")),
    args: () => ({ path: "big.txt" }),
  },
  {
    name: "read-byte-truncation",
    tool: "read",
    setup: (d) => file(d, "big.txt", seq(500, (i) => `Line ${i}: ${"x".repeat(200)}`).join("\n")),
    args: () => ({ path: "big.txt" }),
  },
  {
    name: "read-offset-limit",
    tool: "read",
    setup: (d) => file(d, "a.txt", seq(100, (i) => `Line ${i}`).join("\n")),
    args: () => ({ path: "a.txt", offset: 41, limit: 20 }),
  },
  {
    name: "read-limit-zero",
    tool: "read",
    setup: (d) => file(d, "a.txt", "one\ntwo\nthree"),
    args: () => ({ path: "a.txt", limit: 0 }),
  },
  {
    name: "read-offset-beyond-eof",
    tool: "read",
    setup: (d) => file(d, "a.txt", "Line 1\nLine 2\nLine 3"),
    args: () => ({ path: "a.txt", offset: 100 }),
  },
  {
    name: "read-missing-file",
    tool: "read",
    args: (d) => ({ path: join(d, "nonexistent.txt") }),
  },
  {
    name: "read-first-line-exceeds-limit",
    tool: "read",
    setup: (d) => file(d, "wide.txt", `${"x".repeat(60 * 1024)}\nshort`),
    args: () => ({ path: "wide.txt" }),
  },
  {
    name: "read-cr-only-endings",
    tool: "read",
    setup: (d) => file(d, "cr.txt", "one\rtwo\rthree"),
    args: () => ({ path: "cr.txt" }),
  },
  {
    name: "read-image-png-magic",
    tool: "read",
    setup: (d) => file(d, "image.txt", png1x1),
    args: () => ({ path: "image.txt" }),
  },
  {
    name: "read-image-bmp-conversion",
    tool: "read",
    setup: (d) => file(d, "image.bmp", tinyBmp()),
    args: () => ({ path: "image.bmp" }),
  },
  {
    name: "read-png-extension-text-content",
    tool: "read",
    setup: (d) => file(d, "fake.png", "definitely not a png"),
    args: () => ({ path: "fake.png" }),
  },
  // ----------------------------------------------------------------- write
  {
    name: "write-simple",
    tool: "write",
    args: () => ({ path: "out.txt", content: "Test content" }),
    postFiles: ["out.txt"],
  },
  {
    name: "write-nested-dirs",
    tool: "write",
    args: () => ({ path: "nested/dir/out.txt", content: "Nested content" }),
    postFiles: ["nested/dir/out.txt"],
  },
  {
    name: "write-unicode-utf16-count",
    tool: "write",
    args: () => ({ path: "uni.txt", content: "héllo 🌍 世界\n" }),
    postFiles: ["uni.txt"],
  },
  {
    name: "write-overwrite",
    tool: "write",
    setup: (d) => file(d, "out.txt", "old content"),
    args: () => ({ path: "out.txt", content: "new" }),
    postFiles: ["out.txt"],
  },
  // ------------------------------------------------------------------ edit
  {
    name: "edit-simple",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "Hello, world!"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "world", newText: "testing" }] }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-multi-disjoint",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "alpha\nbeta\ngamma\ndelta\n"),
    args: () => ({
      path: "e.txt",
      edits: [
        { oldText: "alpha\n", newText: "ALPHA\n" },
        { oldText: "gamma\n", newText: "GAMMA\n" },
      ],
    }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-large-gap-diff",
    tool: "edit",
    setup: (d) => file(d, "e.txt", `${seq(600, (i) => `line ${String(i).padStart(3, "0")}`).join("\n")}\n`),
    args: () => ({
      path: "e.txt",
      edits: [
        { oldText: "line 100\n", newText: "LINE 100\n" },
        { oldText: "line 300\n", newText: "LINE 300\n" },
        { oldText: "line 500\n", newText: "LINE 500\n" },
      ],
    }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-fuzzy-trailing-ws",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "line one   \nline two  \nline three\n"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "line one\nline two\n", newText: "replaced\n" }] }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-fuzzy-quotes-dashes-nbsp",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "console.log(‘hi’);\nrange 1–5\nhello world\n"),
    args: () => ({
      path: "e.txt",
      edits: [
        { oldText: "console.log('hi');\n", newText: "console.log('yo');\n" },
        { oldText: "range 1-5\n", newText: "range 1-9\n" },
        { oldText: "hello world\n", newText: "hello universe\n" },
      ],
    }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-fuzzy-nfkc",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "ＡＢＣ１２３\ncafé\n"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "ABC123\ncafé\n", newText: "XYZ789\ncoffee\n" }] }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-fuzzy-preserve-duplicate-line",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "replace me   \nafter   \n"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "replace me\n", newText: "after\n" }] }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-crlf-preserve",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "first\r\nsecond\r\nthird\r\n"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "second\n", newText: "REPLACED\n" }] }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-bom-crlf-multi",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "﻿first\r\nsecond\r\nthird\r\nfourth\r\n"),
    args: () => ({
      path: "e.txt",
      edits: [
        { oldText: "second\n", newText: "SECOND\n" },
        { oldText: "fourth\n", newText: "FOURTH\n" },
      ],
    }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-no-trailing-newline-patch",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "alpha\nomega"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "omega", newText: "OMEGA" }] }),
    postFiles: ["e.txt"],
  },
  {
    name: "edit-not-found-error",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "Hello, world!"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "nonexistent", newText: "x" }] }),
  },
  {
    name: "edit-duplicate-error",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "foo foo foo"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "foo", newText: "bar" }] }),
  },
  {
    name: "edit-overlap-error",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "one\ntwo\nthree\n"),
    args: () => ({
      path: "e.txt",
      edits: [
        { oldText: "one\ntwo\n", newText: "ONE\nTWO\n" },
        { oldText: "two\nthree\n", newText: "TWO\nTHREE\n" },
      ],
    }),
  },
  {
    name: "edit-empty-edits-error",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "hello\n"),
    args: () => ({ path: "e.txt", edits: [] }),
  },
  {
    name: "edit-empty-oldtext-error",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "hello\n"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "", newText: "x" }] }),
  },
  {
    name: "edit-missing-file-error",
    tool: "edit",
    args: (d) => ({ path: join(d, "missing.txt"), edits: [{ oldText: "a", newText: "b" }] }),
  },
  {
    name: "edit-no-change-error",
    tool: "edit",
    setup: (d) => file(d, "e.txt", "same\n"),
    args: () => ({ path: "e.txt", edits: [{ oldText: "same", newText: "same" }] }),
  },
  {
    name: "edit-legacy-oldtext-newtext",
    tool: "edit",
    setup: (d) => file(d, "legacy.txt", "before\n"),
    args: () => ({ path: "legacy.txt", oldText: "before", newText: "after" }),
    postFiles: ["legacy.txt"],
  },
  {
    name: "edit-stringified-edits",
    tool: "edit",
    setup: (d) => file(d, "s.txt", "before\n"),
    args: () => ({ path: "s.txt", edits: JSON.stringify([{ oldText: "before", newText: "after" }]) }),
    postFiles: ["s.txt"],
  },
  // ------------------------------------------------------------------ bash
  {
    name: "bash-echo",
    tool: "bash",
    args: () => ({ command: "echo 'test output'" }),
  },
  {
    name: "bash-no-output",
    tool: "bash",
    args: () => ({ command: "true" }),
  },
  {
    name: "bash-exit-1",
    tool: "bash",
    args: () => ({ command: "exit 1" }),
  },
  {
    name: "bash-exit-7-with-output",
    tool: "bash",
    args: () => ({ command: "echo partial result; exit 7" }),
  },
  {
    name: "bash-stderr-only",
    tool: "bash",
    args: () => ({ command: "echo 'to stderr' 1>&2" }),
  },
  {
    name: "bash-line-truncation",
    tool: "bash",
    args: () => ({ command: "seq 3000" }),
  },
  {
    name: "bash-trailing-newline-count",
    tool: "bash",
    args: () => ({ command: "seq -f 'line-%04.0f' 4000" }),
  },
  {
    name: "bash-single-long-line",
    tool: "bash",
    args: () => ({ command: "printf 'x%.0s' $(seq 1 60000); echo" }),
  },
  {
    name: "bash-invalid-timeout-zero",
    tool: "bash",
    args: () => ({ command: "echo hi", timeout: 0 }),
  },
  {
    name: "bash-invalid-timeout-negative",
    tool: "bash",
    args: () => ({ command: "echo hi", timeout: -5 }),
  },
  {
    name: "bash-timeout-with-output",
    tool: "bash",
    args: () => ({ command: "seq 3000; sleep 5", timeout: 2 }),
  },
  {
    name: "bash-utf8-content",
    tool: "bash",
    args: () => ({ command: "printf '\\xe2\\x82\\xac\\n'" }),
  },
  // ------------------------------------------------------------------ grep
  {
    name: "grep-single-file",
    tool: "grep",
    setup: (d) => file(d, "example.txt", "first line\nmatch line\nlast line"),
    args: (d) => ({ pattern: "match", path: join(d, "example.txt") }),
  },
  {
    name: "grep-dir-relative-paths",
    sortLines: true,
    tool: "grep",
    setup: (d) => {
      file(d, "one.txt", "alpha match\n");
      file(d, "sub/two.txt", "beta match\n");
    },
    args: (d) => ({ pattern: "match", path: d }),
  },
  {
    name: "grep-context",
    tool: "grep",
    setup: (d) => file(d, "ctx.txt", ["before", "match one", "after", "middle", "match two", "after two"].join("\n")),
    args: (d) => ({ pattern: "match", path: join(d, "ctx.txt"), context: 1 }),
  },
  {
    name: "grep-limit-context",
    tool: "grep",
    setup: (d) => file(d, "ctx.txt", ["before", "match one", "after", "middle", "match two", "after two"].join("\n")),
    args: (d) => ({ pattern: "match", path: join(d, "ctx.txt"), limit: 1, context: 1 }),
  },
  {
    name: "grep-ignore-case",
    tool: "grep",
    setup: (d) => file(d, "c.txt", "Match One\nmatch two\nMATCH THREE\n"),
    args: (d) => ({ pattern: "match", path: join(d, "c.txt"), ignoreCase: true }),
  },
  {
    name: "grep-literal",
    tool: "grep",
    setup: (d) => file(d, "l.txt", "a.b*c\nabc\naXbYc\n"),
    args: (d) => ({ pattern: "a.b*c", path: join(d, "l.txt"), literal: true }),
  },
  {
    name: "grep-glob-filter",
    tool: "grep",
    setup: (d) => {
      file(d, "x.ts", "match in ts\n");
      file(d, "y.js", "match in js\n");
    },
    args: (d) => ({ pattern: "match", path: d, glob: "*.ts" }),
  },
  {
    name: "grep-no-matches",
    tool: "grep",
    setup: (d) => file(d, "a.txt", "nothing here\n"),
    args: (d) => ({ pattern: "zzz-not-there", path: d }),
  },
  {
    name: "grep-path-not-found",
    tool: "grep",
    args: (d) => ({ pattern: "x", path: join(d, "missing-dir") }),
  },
  {
    name: "grep-hidden-files",
    sortLines: true,
    tool: "grep",
    setup: (d) => {
      file(d, ".hidden/secret.txt", "match hidden\n");
      file(d, "plain.txt", "match plain\n");
    },
    args: (d) => ({ pattern: "match", path: d }),
  },
  {
    name: "grep-gitignore-outside-repo",
    sortLines: true,
    tool: "grep",
    setup: (d) => {
      file(d, ".gitignore", "*.log\n");
      file(d, "keep.txt", "match keep\n");
      file(d, "skip.log", "match skip\n");
    },
    args: (d) => ({ pattern: "match", path: d }),
  },
  {
    name: "grep-gitignore-inside-repo",
    sortLines: true,
    tool: "grep",
    setup: (d) => {
      mkdirSync(join(d, ".git"), { recursive: true });
      file(d, ".gitignore", "*.log\n");
      file(d, "keep.txt", "match keep\n");
      file(d, "skip.log", "match skip\n");
    },
    args: (d) => ({ pattern: "match", path: d }),
  },
  {
    name: "grep-long-line-truncation",
    tool: "grep",
    setup: (d) => file(d, "long.txt", `prefix match ${"y".repeat(600)}\n`),
    args: (d) => ({ pattern: "match", path: join(d, "long.txt") }),
  },
  {
    name: "grep-crlf-file",
    tool: "grep",
    setup: (d) => file(d, "crlf.txt", "one\r\nmatch two\r\nthree\r\n"),
    args: (d) => ({ pattern: "match", path: join(d, "crlf.txt"), context: 1 }),
  },
  {
    name: "grep-binary-file-skipped",
    sortLines: true,
    tool: "grep",
    setup: (d) => {
      file(d, "bin.dat", Buffer.concat([Buffer.from("match\x00"), Buffer.alloc(64)]));
      file(d, "text.txt", "match text\n");
    },
    args: (d) => ({ pattern: "match", path: d }),
  },
  {
    name: "grep-limit-reached-multifile",
    tool: "grep",
    setup: (d) => {
      file(d, "m.txt", seq(150, (i) => `match ${i}`).join("\n"));
    },
    args: (d) => ({ pattern: "match", path: d, limit: 100 }),
  },
  // ------------------------------------------------------------------ find
  {
    name: "find-basename-glob",
    sortLines: true,
    tool: "find",
    setup: (d) => {
      file(d, "a.txt", "x");
      file(d, "b.md", "x");
      file(d, "sub/c.txt", "x");
    },
    args: (d) => ({ pattern: "*.txt", path: d }),
  },
  {
    name: "find-recursive-glob-hidden-gitignore",
    sortLines: true,
    tool: "find",
    setup: (d) => {
      file(d, ".secret/hidden.txt", "x");
      file(d, "visible.txt", "x");
      file(d, ".gitignore", "ignored.txt\n");
      file(d, "ignored.txt", "x");
    },
    args: (d) => ({ pattern: "**/*.txt", path: d }),
  },
  {
    name: "find-path-pattern",
    sortLines: true,
    tool: "find",
    setup: (d) => {
      file(d, "src/mod/x.spec.ts", "x");
      file(d, "src/y.spec.ts", "x");
      file(d, "other/z.spec.ts", "x");
    },
    args: (d) => ({ pattern: "src/**/*.spec.ts", path: d }),
  },
  {
    name: "find-limit",
    sortLines: true,
    tool: "find",
    setup: (d) => {
      for (let i = 1; i <= 5; i++) file(d, `f${String(i).padStart(2, "0")}.txt`, "x");
      for (let i = 1; i <= 15; i++) file(d, `g${String(i).padStart(2, "0")}.md`, "x");
    },
    args: (d) => ({ pattern: "*.txt", path: d, limit: 5 }),
  },
  {
    name: "find-no-matches",
    tool: "find",
    setup: (d) => file(d, "a.md", "x"),
    args: (d) => ({ pattern: "--help", path: d }),
  },
  {
    name: "find-bad-glob",
    tool: "find",
    setup: (d) => file(d, "a.txt", "x"),
    args: (d) => ({ pattern: "[", path: d }),
  },
  {
    name: "find-directories-match",
    sortLines: true,
    tool: "find",
    setup: (d) => {
      mkdirSync(join(d, "widgets"), { recursive: true });
      file(d, "widgets/w.txt", "x");
      mkdirSync(join(d, "gadgets"), { recursive: true });
    },
    args: (d) => ({ pattern: "*gets", path: d }),
  },
  {
    name: "find-fdignore",
    sortLines: true,
    tool: "find",
    setup: (d) => {
      file(d, ".fdignore", "skipme.txt\n");
      file(d, "skipme.txt", "x");
      file(d, "keep.txt", "x");
    },
    args: (d) => ({ pattern: "*.txt", path: d }),
  },
  {
    name: "find-star-star",
    sortLines: true,
    tool: "find",
    setup: (d) => {
      file(d, "a.txt", "x");
      file(d, "sub/b.txt", "x");
    },
    args: (d) => ({ pattern: "**", path: d }),
  },
  // -------------------------------------------------------------------- ls
  {
    name: "ls-dotfiles-and-dirs",
    tool: "ls",
    setup: (d) => {
      file(d, ".hidden-file", "secret");
      mkdirSync(join(d, ".hidden-dir"), { recursive: true });
      file(d, "regular.txt", "x");
      mkdirSync(join(d, "subdir"), { recursive: true });
    },
    args: (d) => ({ path: d }),
  },
  {
    name: "ls-empty",
    tool: "ls",
    args: (d) => ({ path: d }),
  },
  {
    name: "ls-limit",
    tool: "ls",
    setup: (d) => {
      for (let i = 1; i <= 10; i++) file(d, `f${String(i).padStart(2, "0")}.txt`, "x");
    },
    args: (d) => ({ path: d, limit: 5 }),
  },
  {
    name: "ls-not-a-directory",
    tool: "ls",
    setup: (d) => file(d, "plain.txt", "x"),
    args: (d) => ({ path: join(d, "plain.txt") }),
  },
  {
    name: "ls-missing",
    tool: "ls",
    args: (d) => ({ path: join(d, "nope") }),
  },
  {
    name: "ls-broken-symlink-skipped",
    tool: "ls",
    setup: (d) => {
      file(d, "real.txt", "x");
      symlinkSync(join(d, "does-not-exist"), join(d, "dangling"));
    },
    args: (d) => ({ path: d }),
  },
  {
    name: "ls-mixed-case-sort",
    tool: "ls",
    setup: (d) => {
      for (const name of ["Zebra.txt", "apple.txt", "Banana.txt", "_underscore", ".dot", "10-num", "2-num"]) {
        file(d, name, "x");
      }
    },
    args: (d) => ({ path: d }),
  },
];

// ------------------------------------------------------------------ runner

const workRoot = join(process.env.TMPDIR ?? "/tmp", `pi-parity-${process.pid}`);
rmSync(workRoot, { recursive: true, force: true });
mkdirSync(workRoot, { recursive: true });

let pass = 0;
let fail = 0;
const failures: string[] = [];

for (const c of cases) {
  if (filter && !c.name.includes(filter)) continue;
  if (c.skip) {
    console.log(`SKIP ${c.name}: ${c.skip}`);
    continue;
  }

  const dirPi = join(workRoot, `${c.name}-pi`);
  const dirRs = join(workRoot, `${c.name}-rs`);
  mkdirSync(dirPi, { recursive: true });
  mkdirSync(dirRs, { recursive: true });
  c.setup?.(dirPi);
  c.setup?.(dirRs);

  // pi spawns rg/fd from the process cwd; align both sides on the case dir.
  process.chdir(dirPi);
  const piEnv = await runPi(c, dirPi);
  process.chdir(workRoot);
  const rsEnv = await runRust(c, dirRs);

  if (c.sortLines) {
    sortBodyLines(piEnv);
    sortBodyLines(rsEnv);
  }
  const diffs = deepDiff(piEnv, rsEnv);
  if (diffs.length === 0) {
    pass++;
    console.log(`PASS ${c.name}`);
  } else {
    fail++;
    console.log(`FAIL ${c.name}`);
    for (const d of diffs.slice(0, 12)) console.log(`     ${d}`);
    if (diffs.length > 12) console.log(`     ... ${diffs.length - 12} more`);
    failures.push(c.name);
  }
}

console.log(`\n${pass} passed, ${fail} failed${fail ? `: ${failures.join(", ")}` : ""}`);
process.exit(fail === 0 ? 0 : 1);
