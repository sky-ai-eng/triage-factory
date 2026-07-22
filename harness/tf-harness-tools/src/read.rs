//! Port of pi's `core/tools/read.ts` execute path.

use serde::Deserialize;

use crate::errors::node_fs_error;
use crate::image::{detect_supported_image_mime_type_from_file, process_image, ProcessImageResult};
use crate::jsnum::{js_num, to_index};
use crate::paths::resolve_read_path;
use crate::result::{ContentBlock, ReadToolDetails, ToolError, ToolResult, ToolResultOrError};
use crate::truncate::{format_size, truncate_head, TruncationOptions, DEFAULT_MAX_BYTES};

#[derive(Debug, Clone, Deserialize)]
pub struct ReadArgs {
    pub path: String,
    #[serde(default)]
    pub offset: Option<f64>,
    #[serde(default)]
    pub limit: Option<f64>,
}

#[derive(Debug, Clone, Copy)]
pub struct ReadOptions {
    /// pi default: true.
    pub auto_resize_images: bool,
    /// None → no model context (pi omits the note); Some(false) → append the
    /// non-vision note to image reads.
    pub model_supports_images: Option<bool>,
}

impl Default for ReadOptions {
    fn default() -> Self {
        ReadOptions {
            auto_resize_images: true,
            model_supports_images: None,
        }
    }
}

const NON_VISION_NOTE: &str =
    "[Current model does not support images. The image will be omitted from this request.]";

fn access_readable(path: &str) -> Result<(), ToolError> {
    let c_path = std::ffi::CString::new(path.as_bytes()).map_err(|_| {
        ToolError::new(format!(
            "ENOENT: no such file or directory, access '{path}'"
        ))
    })?;
    let rc = unsafe { libc::access(c_path.as_ptr(), libc::R_OK) };
    if rc == 0 {
        Ok(())
    } else {
        let err = std::io::Error::last_os_error();
        Err(ToolError::new(node_fs_error(&err, "access", path).message))
    }
}

pub fn execute(
    cwd: &str,
    args: &ReadArgs,
    options: &ReadOptions,
) -> ToolResultOrError<ReadToolDetails> {
    let absolute_path = resolve_read_path(&args.path, cwd);
    access_readable(&absolute_path)?;

    let non_vision_note = match options.model_supports_images {
        Some(false) => Some(NON_VISION_NOTE),
        _ => None,
    };

    let mime_type = detect_supported_image_mime_type_from_file(&absolute_path)
        .map_err(|e| ToolError::new(node_fs_error(&e, "open", &absolute_path).message))?;

    if let Some(mime_type) = mime_type {
        let buffer = std::fs::read(&absolute_path)
            .map_err(|e| ToolError::new(node_fs_error(&e, "open", &absolute_path).message))?;
        let content = match process_image(&buffer, mime_type, options.auto_resize_images) {
            ProcessImageResult::Err { message } => {
                let mut text_note = format!("Read image file [{mime_type}]\n{message}");
                if let Some(note) = non_vision_note {
                    text_note.push('\n');
                    text_note.push_str(note);
                }
                vec![ContentBlock::text(text_note)]
            }
            ProcessImageResult::Ok {
                data,
                mime_type,
                hints,
            } => {
                let mut text_note = format!("Read image file [{mime_type}]");
                if !hints.is_empty() {
                    text_note.push('\n');
                    text_note.push_str(&hints.join("\n"));
                }
                if let Some(note) = non_vision_note {
                    text_note.push('\n');
                    text_note.push_str(note);
                }
                vec![
                    ContentBlock::text(text_note),
                    ContentBlock::Image { data, mime_type },
                ]
            }
        };
        return Ok(ToolResult {
            content,
            details: None,
        });
    }

    // Text content: lossy UTF-8 decode, exactly like Buffer.toString("utf-8").
    let buffer = std::fs::read(&absolute_path)
        .map_err(|e| ToolError::new(node_fs_error(&e, "open", &absolute_path).message))?;
    let text_content = String::from_utf8_lossy(&buffer);
    let all_lines: Vec<&str> = text_content.split('\n').collect();
    let total_file_lines = all_lines.len();

    // 1-indexed offset; JS `offset ? max(0, offset - 1) : 0` (0 is falsy).
    let offset_raw = args.offset.unwrap_or(0.0);
    let start_line_f = if offset_raw != 0.0 && !offset_raw.is_nan() {
        (offset_raw - 1.0).max(0.0)
    } else {
        0.0
    };
    if start_line_f >= all_lines.len() as f64 {
        return Err(ToolError::new(format!(
            "Offset {} is beyond end of file ({} lines total)",
            js_num(offset_raw),
            all_lines.len()
        )));
    }
    let start_line = to_index(start_line_f);
    let start_line_display = start_line + 1;

    let mut user_limited_lines: Option<usize> = None;
    let selected_content: String;
    if let Some(limit) = args.limit {
        let end_line = ((start_line as f64) + limit).min(all_lines.len() as f64);
        let end_line = to_index(end_line).min(all_lines.len());
        let end_line = end_line.max(start_line);
        selected_content = all_lines[start_line..end_line].join("\n");
        user_limited_lines = Some(end_line - start_line);
    } else {
        selected_content = all_lines[start_line..].join("\n");
    }

    let truncation = truncate_head(&selected_content, TruncationOptions::default());
    let mut details: Option<ReadToolDetails> = None;
    let output_text: String;
    if truncation.first_line_exceeds_limit {
        // First line alone exceeds the byte limit; point the model at a bash
        // fallback.
        let first_line_size = format_size(all_lines[start_line].len());
        output_text = format!(
            "[Line {start_line_display} is {first_line_size}, exceeds {} limit. Use bash: sed -n '{start_line_display}p' {} | head -c {}]",
            format_size(DEFAULT_MAX_BYTES),
            args.path,
            DEFAULT_MAX_BYTES
        );
        details = Some(ReadToolDetails {
            truncation: Some(truncation),
        });
    } else if truncation.truncated {
        let end_line_display = start_line_display + truncation.output_lines - 1;
        let next_offset = end_line_display + 1;
        let mut text = truncation.content.clone();
        if truncation.truncated_by == Some("lines") {
            text.push_str(&format!(
                "\n\n[Showing lines {start_line_display}-{end_line_display} of {total_file_lines}. Use offset={next_offset} to continue.]"
            ));
        } else {
            text.push_str(&format!(
                "\n\n[Showing lines {start_line_display}-{end_line_display} of {total_file_lines} ({} limit). Use offset={next_offset} to continue.]",
                format_size(DEFAULT_MAX_BYTES)
            ));
        }
        output_text = text;
        details = Some(ReadToolDetails {
            truncation: Some(truncation),
        });
    } else if let Some(limited) = user_limited_lines.filter(|l| start_line + l < all_lines.len()) {
        let remaining = all_lines.len() - (start_line + limited);
        let next_offset = start_line + limited + 1;
        output_text = format!(
            "{}\n\n[{remaining} more lines in file. Use offset={next_offset} to continue.]",
            truncation.content
        );
    } else {
        output_text = truncation.content;
    }

    Ok(ToolResult {
        content: vec![ContentBlock::text(output_text)],
        details,
    })
}
