//! Rust port of pi's seven coding-agent tools (read, bash, edit, write,
//! grep, find, ls) with behavioral parity to
//! `pi/packages/coding-agent/src/core/tools/*` as the hard constraint,
//! optimized for low syscall volume inside gVisor sandboxes: no JS runtime,
//! and no rg/fd subprocesses (ripgrep's crates are embedded).
//!
//! Ported from pi (https://github.com/earendil-works/pi), MIT License,
//! Copyright (c) 2025 Mario Zechner. See LICENSE-pi.

pub mod bash;
pub mod definitions;
pub mod edit;
pub mod edit_diff;
pub mod errors;
pub mod find;
pub mod grep;
pub mod image;
pub mod jsdiff;
pub mod jsnum;
pub mod jsstring;
pub mod ls;
pub mod mutation_queue;
pub mod output_accumulator;
pub mod paths;
pub mod read;
pub mod result;
pub mod serve;
pub mod shell;
pub mod truncate;
pub mod validate;
pub mod write;

use result::ToolError;
use serde_json::Value;

/// Execution policy a caller may impose on the tools — never a model-visible
/// argument, and never part of a tool's JSON schema.
///
/// It exists for the resident host, whose peer configures one session's limits
/// over the socket before the first tool call (see [`serve`]). The one-shot CLI
/// has no such channel and uses the default, so every value here is off unless
/// something deliberately set it.
#[derive(Debug, Clone, Copy, Default)]
pub struct ToolConfig {
    /// Per-command resident-memory budget for `bash`, in MB. Zero disables the
    /// watchdog entirely.
    pub bash_mem_budget_mb: u64,
}

/// Execute a tool by name against JSON args, returning the pi-shaped result
/// (`{content, details?}`) as JSON. Errors carry the exact message pi's
/// tools throw; argument-shape errors are validated against the exported
/// schemas first so the model sees `"offset" must be a number`, never
/// deserializer internals.
///
/// This is the unconfigured entry point — the one-shot CLI's and the parity
/// harness's. A caller holding session policy uses [`run_tool_configured`].
pub fn run_tool(name: &str, cwd: &str, args: Value) -> Result<Value, ToolError> {
    run_tool_configured(name, cwd, args, ToolConfig::default())
}

/// [`run_tool`] under a session's execution policy. Identical in every respect
/// a tool's result can express; `config` only bounds what a tool may consume
/// while producing it.
pub fn run_tool_configured(
    name: &str,
    cwd: &str,
    args: Value,
    config: ToolConfig,
) -> Result<Value, ToolError> {
    let to_json = |v: Result<Value, serde_json::Error>| {
        v.map_err(|e| ToolError::new(format!("Internal serialization error: {e}")))
    };
    // Unreachable once validate_args passes; kept terse and internals-free.
    let bad_args = |_e: serde_json::Error| ToolError::new("Invalid arguments");

    match name {
        "read" => {
            validate::validate_args(name, &args)?;
            let parsed: read::ReadArgs = serde_json::from_value(args).map_err(bad_args)?;
            let result = read::execute(cwd, &parsed, &read::ReadOptions::default())?;
            to_json(serde_json::to_value(result))
        }
        "bash" => {
            validate::validate_args(name, &args)?;
            let parsed: bash::BashArgs = serde_json::from_value(args).map_err(bad_args)?;
            let options = bash::BashOptions {
                mem_budget_mb: config.bash_mem_budget_mb,
                ..Default::default()
            };
            let result = bash::execute(cwd, &parsed, &options)?;
            to_json(serde_json::to_value(result))
        }
        "edit" => {
            // Like pi's runtime: prepareArguments folds legacy shapes first,
            // then the schema validates, then execute.
            let prepared = edit::prepare_arguments(args);
            validate::validate_args(name, &prepared)?;
            let parsed = edit::parse_args(&prepared)?;
            let result = edit::execute(cwd, &parsed)?;
            to_json(serde_json::to_value(result))
        }
        "write" => {
            validate::validate_args(name, &args)?;
            let parsed: write::WriteArgs = serde_json::from_value(args).map_err(bad_args)?;
            let result = write::execute(cwd, &parsed)?;
            to_json(serde_json::to_value(result))
        }
        "grep" => {
            validate::validate_args(name, &args)?;
            let parsed: grep::GrepArgs = serde_json::from_value(args).map_err(bad_args)?;
            let result = grep::execute(cwd, &parsed)?;
            to_json(serde_json::to_value(result))
        }
        "find" => {
            validate::validate_args(name, &args)?;
            let parsed: find::FindArgs = serde_json::from_value(args).map_err(bad_args)?;
            let result = find::execute(cwd, &parsed)?;
            to_json(serde_json::to_value(result))
        }
        "ls" => {
            validate::validate_args(name, &args)?;
            let parsed: ls::LsArgs = serde_json::from_value(args).map_err(bad_args)?;
            let result = ls::execute(cwd, &parsed)?;
            to_json(serde_json::to_value(result))
        }
        other => Err(ToolError::new(format!("Unknown tool name: {other}"))),
    }
}
