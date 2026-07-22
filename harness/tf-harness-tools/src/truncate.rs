//! Port of pi's `core/tools/truncate.ts`.
//!
//! Truncation is based on two independent limits — whichever is hit first
//! wins: a line limit (default 2000) and a byte limit (default 50KB). Never
//! returns partial lines, except the bash tail-truncation edge case.

use serde::Serialize;

use crate::jsstring::{utf16_len, utf16_slice_to};

pub const DEFAULT_MAX_LINES: usize = 2000;
pub const DEFAULT_MAX_BYTES: usize = 50 * 1024;
pub const GREP_MAX_LINE_LENGTH: usize = 500;

#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct TruncationResult {
    pub content: String,
    pub truncated: bool,
    /// "lines", "bytes", or None when not truncated.
    pub truncated_by: Option<&'static str>,
    pub total_lines: usize,
    pub total_bytes: usize,
    pub output_lines: usize,
    pub output_bytes: usize,
    pub last_line_partial: bool,
    pub first_line_exceeds_limit: bool,
    pub max_lines: usize,
    pub max_bytes: usize,
}

#[derive(Debug, Clone, Copy, Default)]
pub struct TruncationOptions {
    pub max_lines: Option<usize>,
    pub max_bytes: Option<usize>,
}

/// JS `content.split("\n")` with a trailing empty element popped when the
/// content ends in a newline; empty content counts as zero lines.
fn split_lines_for_counting(content: &str) -> Vec<&str> {
    if content.is_empty() {
        return Vec::new();
    }
    let mut lines: Vec<&str> = content.split('\n').collect();
    if content.ends_with('\n') {
        lines.pop();
    }
    lines
}

/// Human-readable size with JS `toFixed(1)` semantics.
pub fn format_size(bytes: usize) -> String {
    if bytes < 1024 {
        format!("{bytes}B")
    } else if bytes < 1024 * 1024 {
        format!("{:.1}KB", bytes as f64 / 1024.0)
    } else {
        format!("{:.1}MB", bytes as f64 / (1024.0 * 1024.0))
    }
}

/// Keep the first N lines/bytes. Never returns partial lines; if the first
/// line alone exceeds the byte limit, returns empty content with
/// `first_line_exceeds_limit`.
pub fn truncate_head(content: &str, options: TruncationOptions) -> TruncationResult {
    let max_lines = options.max_lines.unwrap_or(DEFAULT_MAX_LINES);
    let max_bytes = options.max_bytes.unwrap_or(DEFAULT_MAX_BYTES);

    let total_bytes = content.len();
    let lines = split_lines_for_counting(content);
    let total_lines = lines.len();

    if total_lines <= max_lines && total_bytes <= max_bytes {
        return TruncationResult {
            content: content.to_string(),
            truncated: false,
            truncated_by: None,
            total_lines,
            total_bytes,
            output_lines: total_lines,
            output_bytes: total_bytes,
            last_line_partial: false,
            first_line_exceeds_limit: false,
            max_lines,
            max_bytes,
        };
    }

    let first_line_bytes = lines.first().map_or(0, |l| l.len());
    if first_line_bytes > max_bytes {
        return TruncationResult {
            content: String::new(),
            truncated: true,
            truncated_by: Some("bytes"),
            total_lines,
            total_bytes,
            output_lines: 0,
            output_bytes: 0,
            last_line_partial: false,
            first_line_exceeds_limit: true,
            max_lines,
            max_bytes,
        };
    }

    let mut output_lines_arr: Vec<&str> = Vec::new();
    let mut output_bytes_count = 0usize;
    let mut truncated_by = "lines";

    for (i, line) in lines.iter().enumerate() {
        if i >= max_lines {
            break;
        }
        let line_bytes = line.len() + usize::from(i > 0);
        if output_bytes_count + line_bytes > max_bytes {
            truncated_by = "bytes";
            break;
        }
        output_lines_arr.push(line);
        output_bytes_count += line_bytes;
    }

    if output_lines_arr.len() >= max_lines && output_bytes_count <= max_bytes {
        truncated_by = "lines";
    }

    let output_content = output_lines_arr.join("\n");
    let final_output_bytes = output_content.len();

    TruncationResult {
        truncated: true,
        truncated_by: Some(if truncated_by == "bytes" {
            "bytes"
        } else {
            "lines"
        }),
        total_lines,
        total_bytes,
        output_lines: output_lines_arr.len(),
        output_bytes: final_output_bytes,
        last_line_partial: false,
        first_line_exceeds_limit: false,
        max_lines,
        max_bytes,
        content: output_content,
    }
}

/// Keep the last N lines/bytes. May return a partial first line when the last
/// line of the original content alone exceeds the byte limit.
pub fn truncate_tail(content: &str, options: TruncationOptions) -> TruncationResult {
    let max_lines = options.max_lines.unwrap_or(DEFAULT_MAX_LINES);
    let max_bytes = options.max_bytes.unwrap_or(DEFAULT_MAX_BYTES);

    let total_bytes = content.len();
    let lines = split_lines_for_counting(content);
    let total_lines = lines.len();

    if total_lines <= max_lines && total_bytes <= max_bytes {
        return TruncationResult {
            content: content.to_string(),
            truncated: false,
            truncated_by: None,
            total_lines,
            total_bytes,
            output_lines: total_lines,
            output_bytes: total_bytes,
            last_line_partial: false,
            first_line_exceeds_limit: false,
            max_lines,
            max_bytes,
        };
    }

    let mut output_lines_arr: Vec<&str> = Vec::new();
    let mut output_bytes_count = 0usize;
    let mut truncated_by = "lines";
    let mut last_line_partial = false;
    let partial_line_storage;

    for line in lines.iter().rev() {
        if output_lines_arr.len() >= max_lines {
            break;
        }
        let line_bytes = line.len() + usize::from(!output_lines_arr.is_empty());
        if output_bytes_count + line_bytes > max_bytes {
            truncated_by = "bytes";
            if output_lines_arr.is_empty() {
                partial_line_storage = truncate_str_to_bytes_from_end(line, max_bytes).to_string();
                output_bytes_count = partial_line_storage.len();
                output_lines_arr.push(&partial_line_storage);
                last_line_partial = true;
            }
            break;
        }
        output_lines_arr.push(line);
        output_bytes_count += line_bytes;
    }
    output_lines_arr.reverse();

    if output_lines_arr.len() >= max_lines && output_bytes_count <= max_bytes {
        truncated_by = "lines";
    }

    let output_content = output_lines_arr.join("\n");
    let final_output_bytes = output_content.len();
    let output_line_count = output_lines_arr.len();

    TruncationResult {
        truncated: true,
        truncated_by: Some(if truncated_by == "bytes" {
            "bytes"
        } else {
            "lines"
        }),
        total_lines,
        total_bytes,
        output_lines: output_line_count,
        output_bytes: final_output_bytes,
        last_line_partial,
        first_line_exceeds_limit: false,
        max_lines,
        max_bytes,
        content: output_content,
    }
}

/// Trim a string to fit a byte budget from the end, starting at a valid UTF-8
/// character boundary.
pub fn truncate_str_to_bytes_from_end(s: &str, max_bytes: usize) -> &str {
    let bytes = s.as_bytes();
    if bytes.len() <= max_bytes {
        return s;
    }
    let mut start = bytes.len() - max_bytes;
    while start < bytes.len() && (bytes[start] & 0xc0) == 0x80 {
        start += 1;
    }
    &s[start..]
}

/// Truncate a single line to `max_chars` UTF-16 code units, adding a
/// `... [truncated]` suffix. Used for grep match lines.
pub fn truncate_line(line: &str, max_chars: usize) -> (String, bool) {
    if utf16_len(line) <= max_chars {
        return (line.to_string(), false);
    }
    (
        format!("{}... [truncated]", utf16_slice_to(line, max_chars)),
        true,
    )
}
