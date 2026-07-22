#!/usr/bin/env bun
/**
 * Minimal pi-side driver for syscall benchmarking: run one pi tool N times.
 *   bun pi-one.ts <tool> <cwd> <args-json> [repeat]
 */
import { join } from "node:path";

const piRoot = process.env.PI_ROOT ?? new URL("../../../pi", `file://${import.meta.dir}/`).pathname;
const [tool, cwd, argsJson, repeatStr] = process.argv.slice(2);
const repeat = Number(repeatStr ?? "1");

const toolsModule = await import(join(piRoot, "packages/coding-agent/src/core/tools/index.ts"));
const t = toolsModule.createTool(tool, cwd);
const args = JSON.parse(argsJson);

let last: unknown;
for (let i = 0; i < repeat; i++) {
  try {
    const prepared = t.prepareArguments ? t.prepareArguments(structuredClone(args)) : args;
    last = { ok: true, result: await t.execute(`bench-${i}`, prepared) };
  } catch (err) {
    last = { ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}
console.log(JSON.stringify(last).slice(0, 400));
