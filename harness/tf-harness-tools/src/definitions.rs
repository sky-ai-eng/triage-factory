//! Model-facing tool definitions: names, descriptions, and JSON parameter
//! schemas for the seven tools, matching pi's TypeBox output except where
//! noted below. The agent's behavior depends on these strings, so they are
//! part of the parity contract, and a Go-side test compares this whole
//! document against the registry the model is actually served.
//!
//! These are not documentation. [`crate::validate::validate_args`] checks
//! every call against the schema here before dispatch, so a parameter is
//! declared only if a wrong-typed value for it should fail the call — which
//! is the rule even for a parameter no tool implementation reads.
//!
//! Divergence from pi: `bash` carries `description` and `description_past`
//! beyond pi's `{command, timeout}`. A shell command is the only argument in
//! this set that is not legible to a person, so the model authors a summary
//! to render in its place. Nothing here executes them.

use serde_json::{json, Value};

pub fn tool_definition(name: &str) -> Option<Value> {
    let def = match name {
        "read" => json!({
            "name": "read",
            "description": "Read the contents of a file. Supports text files and images (jpg, png, gif, webp, bmp). Images are sent as attachments. For text files, output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": { "description": "Path to the file to read (relative or absolute)", "type": "string" },
                    "offset": { "description": "Line number to start reading from (1-indexed)", "type": "number" },
                    "limit": { "description": "Maximum number of lines to read", "type": "number" }
                },
                "required": ["path"]
            }
        }),
        "bash" => json!({
            "name": "bash",
            "description": "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.",
            "parameters": {
                "type": "object",
                "properties": {
                    "command": { "description": "Bash command to execute", "type": "string" },
                    "timeout": { "description": "Timeout in seconds (optional, no default timeout)", "type": "number" },
                    "description": { "description": "What this command is doing, in 1-6 words and present tense: \"Reproducing the flake\", \"Vetting the sandbox package\". Describe the action, never its outcome.", "type": "string" },
                    "description_past": { "description": "The same summary in past tense, in 1-6 words: \"Ran the sampler test 50x\". You are writing it before the command runs, so describe the action, never a result you cannot know yet.", "type": "string" }
                },
                "required": ["command"]
            }
        }),
        "edit" => json!({
            "name": "edit",
            "description": "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": { "description": "Path to the file to edit (relative or absolute)", "type": "string" },
                    "edits": {
                        "description": "One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead.",
                        "type": "array",
                        "items": {
                            "type": "object",
                            "properties": {
                                "oldText": { "description": "Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call.", "type": "string" },
                                "newText": { "description": "Replacement text for this targeted edit.", "type": "string" }
                            },
                            "required": ["oldText", "newText"]
                        }
                    }
                },
                "required": ["path", "edits"]
            }
        }),
        "write" => json!({
            "name": "write",
            "description": "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": { "description": "Path to the file to write (relative or absolute)", "type": "string" },
                    "content": { "description": "Content to write to the file", "type": "string" }
                },
                "required": ["path", "content"]
            }
        }),
        "grep" => json!({
            "name": "grep",
            "description": "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore. Output is truncated to 100 matches or 50KB (whichever is hit first). Long lines are truncated to 500 chars.",
            "parameters": {
                "type": "object",
                "properties": {
                    "pattern": { "description": "Search pattern (regex or literal string)", "type": "string" },
                    "path": { "description": "Directory or file to search (default: current directory)", "type": "string" },
                    "glob": { "description": "Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'", "type": "string" },
                    "ignoreCase": { "description": "Case-insensitive search (default: false)", "type": "boolean" },
                    "literal": { "description": "Treat pattern as literal string instead of regex (default: false)", "type": "boolean" },
                    "context": { "description": "Number of lines to show before and after each match (default: 0)", "type": "number" },
                    "limit": { "description": "Maximum number of matches to return (default: 100)", "type": "number" }
                },
                "required": ["pattern"]
            }
        }),
        "find" => json!({
            "name": "find",
            "description": "Search for files by glob pattern. Returns matching file paths relative to the search directory. Respects .gitignore. Output is truncated to 1000 results or 50KB (whichever is hit first).",
            "parameters": {
                "type": "object",
                "properties": {
                    "pattern": { "description": "Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'", "type": "string" },
                    "path": { "description": "Directory to search in (default: current directory)", "type": "string" },
                    "limit": { "description": "Maximum number of results (default: 1000)", "type": "number" }
                },
                "required": ["pattern"]
            }
        }),
        "ls" => json!({
            "name": "ls",
            "description": "List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to 500 entries or 50KB (whichever is hit first).",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": { "description": "Directory to list (default: current directory)", "type": "string" },
                    "limit": { "description": "Maximum number of entries to return (default: 500)", "type": "number" }
                }
            }
        }),
        _ => return None,
    };
    Some(def)
}

pub const ALL_TOOL_NAMES: [&str; 7] = ["read", "bash", "edit", "write", "grep", "find", "ls"];

pub fn all_tool_definitions() -> Vec<Value> {
    ALL_TOOL_NAMES
        .iter()
        .map(|n| tool_definition(n).unwrap())
        .collect()
}
