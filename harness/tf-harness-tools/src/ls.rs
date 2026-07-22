//! Port of pi's `core/tools/ls.ts` execute path.

use serde::Deserialize;

use crate::errors::node_fs_error;
use crate::jsnum::js_num;
use crate::paths::{node_join, path_exists, resolve_to_cwd};
use crate::result::{
    ContentBlock, EmptyCheck, LsToolDetails, ToolError, ToolResult, ToolResultOrError,
};
use crate::truncate::{format_size, truncate_head, TruncationOptions, DEFAULT_MAX_BYTES};

pub const DEFAULT_LIMIT: f64 = 500.0;

#[derive(Debug, Clone, Deserialize, Default)]
pub struct LsArgs {
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub limit: Option<f64>,
}

/// Primary collation rank for ASCII, matching the CLDR root collation ICU
/// uses for `localeCompare`: whitespace, then punctuation (`_` before `-`
/// before `.` etc.), then symbols, then digits, then letters.
fn ascii_collation_rank(ch: char) -> u32 {
    const ORDER: &[u8] = b" _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$";
    if let Some(pos) = ORDER.iter().position(|c| *c as char == ch) {
        return 100 + pos as u32;
    }
    match ch {
        '0'..='9' => 200 + (ch as u32 - '0' as u32),
        'a'..='z' => 300 + (ch as u32 - 'a' as u32),
        // Control characters: before everything, in code-point order.
        c if (c as u32) < 0x20 || c as u32 == 0x7f => c as u32,
        // Non-ASCII: after ASCII, in code-point order. ICU interleaves
        // accented letters with their base (é ≈ e); pi's ordering there is
        // host-locale-dependent anyway, so this stays an approximation.
        c => 0x1000 + c as u32,
    }
}

/// JS `a.toLowerCase().localeCompare(b.toLowerCase())` under the root
/// collation, restricted to primary weights (case is already folded).
fn ls_compare(a: &str, b: &str) -> std::cmp::Ordering {
    let a = a.to_lowercase();
    let b = b.to_lowercase();
    let ranks = |s: &str| s.chars().map(ascii_collation_rank).collect::<Vec<_>>();
    ranks(&a).cmp(&ranks(&b))
}

pub fn execute(cwd: &str, args: &LsArgs) -> ToolResultOrError<LsToolDetails> {
    let path_arg = args.path.as_deref().unwrap_or("");
    let dir_path = resolve_to_cwd(if path_arg.is_empty() { "." } else { path_arg }, cwd);
    let effective_limit = args.limit.unwrap_or(DEFAULT_LIMIT);

    if !path_exists(&dir_path) {
        return Err(ToolError::new(format!("Path not found: {dir_path}")));
    }

    let stat = std::fs::metadata(&dir_path)
        .map_err(|e| ToolError::new(node_fs_error(&e, "stat", &dir_path).message))?;
    if !stat.is_dir() {
        return Err(ToolError::new(format!("Not a directory: {dir_path}")));
    }

    let mut entries: Vec<String> = match std::fs::read_dir(&dir_path) {
        Ok(rd) => {
            let mut names = Vec::new();
            for dirent in rd {
                match dirent {
                    Ok(d) => names.push(d.file_name().to_string_lossy().into_owned()),
                    Err(e) => {
                        return Err(ToolError::new(format!(
                            "Cannot read directory: {}",
                            node_fs_error(&e, "scandir", &dir_path).message
                        )))
                    }
                }
            }
            names
        }
        Err(e) => {
            return Err(ToolError::new(format!(
                "Cannot read directory: {}",
                node_fs_error(&e, "scandir", &dir_path).message
            )))
        }
    };

    entries.sort_by(|a, b| ls_compare(a, b));

    let mut results: Vec<String> = Vec::new();
    let mut entry_limit_reached = false;
    for entry in &entries {
        if results.len() as f64 >= effective_limit {
            entry_limit_reached = true;
            break;
        }
        let full_path = node_join(&dir_path, entry);
        // Skip entries we cannot stat (e.g. broken symlinks), like pi.
        match std::fs::metadata(&full_path) {
            Ok(meta) => {
                let suffix = if meta.is_dir() { "/" } else { "" };
                results.push(format!("{entry}{suffix}"));
            }
            Err(_) => continue,
        }
    }

    if results.is_empty() {
        return Ok(ToolResult::text_only("(empty directory)"));
    }

    let raw_output = results.join("\n");
    // Byte truncation only; the entry count is already capped.
    let truncation = truncate_head(
        &raw_output,
        TruncationOptions {
            max_lines: Some(usize::MAX),
            max_bytes: None,
        },
    );
    let mut output = truncation.content.clone();
    let mut details = LsToolDetails {
        truncation: None,
        entry_limit_reached: None,
    };
    let mut notices: Vec<String> = Vec::new();
    if entry_limit_reached {
        notices.push(format!(
            "{} entries limit reached. Use limit={} for more",
            js_num(effective_limit),
            js_num(effective_limit * 2.0)
        ));
        details.entry_limit_reached = Some(effective_limit as usize);
    }
    if truncation.truncated {
        notices.push(format!("{} limit reached", format_size(DEFAULT_MAX_BYTES)));
        details.truncation = Some(truncation);
    }
    if !notices.is_empty() {
        output.push_str(&format!("\n\n[{}]", notices.join(". ")));
    }

    Ok(ToolResult {
        content: vec![ContentBlock::text(output)],
        details: if details.is_empty_details() {
            None
        } else {
            Some(details)
        },
    })
}
