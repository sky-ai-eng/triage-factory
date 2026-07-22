//! Port of pi's `core/tools/grep.ts` execute path, with the `rg` subprocess
//! replaced by ripgrep's own crates (grep-regex + grep-searcher + ignore),
//! configured to match the exact invocation pi uses:
//! `rg --json --line-number --color=never --hidden [--ignore-case]
//! [--fixed-strings] [--glob G] -- PATTERN SEARCHPATH`.
//! Verified against rg 14.1.1: --hidden does include .git, and .gitignore
//! applies only inside git repositories (require_git).

use std::collections::HashMap;

use grep_searcher::{BinaryDetection, Searcher, SearcherBuilder, Sink, SinkMatch};
use ignore::overrides::OverrideBuilder;
use ignore::WalkBuilder;
use serde::Deserialize;

use crate::jsnum::js_num;
use crate::paths::{node_basename, node_relative, resolve_to_cwd};
use crate::result::{
    ContentBlock, EmptyCheck, GrepToolDetails, ToolError, ToolResult, ToolResultOrError,
};
use crate::truncate::{
    format_size, truncate_head, truncate_line, TruncationOptions, DEFAULT_MAX_BYTES,
    GREP_MAX_LINE_LENGTH,
};

pub const DEFAULT_LIMIT: f64 = 100.0;

#[derive(Debug, Clone, Deserialize)]
pub struct GrepArgs {
    pub pattern: String,
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub glob: Option<String>,
    #[serde(default, rename = "ignoreCase")]
    pub ignore_case: Option<bool>,
    #[serde(default)]
    pub literal: Option<bool>,
    #[serde(default)]
    pub context: Option<f64>,
    #[serde(default)]
    pub limit: Option<f64>,
}

struct CollectedMatch {
    file_path: String,
    line_number: u64,
    /// Valid-UTF-8 line content (rg JSON `lines.text`); None mirrors rg's
    /// base64 fallback for invalid UTF-8, which pi handles by re-reading the
    /// file.
    line_text: Option<String>,
}

struct MatchSink<'a> {
    file_path: String,
    matches: &'a mut Vec<CollectedMatch>,
    match_count: &'a mut usize,
    effective_limit: f64,
    match_limit_reached: &'a mut bool,
}

impl Sink for MatchSink<'_> {
    type Error = std::io::Error;

    fn matched(
        &mut self,
        _searcher: &Searcher,
        mat: &SinkMatch<'_>,
    ) -> Result<bool, std::io::Error> {
        if (*self.match_count as f64) >= self.effective_limit {
            return Ok(false);
        }
        *self.match_count += 1;
        let line_number = mat.line_number().unwrap_or(0);
        let line_text = std::str::from_utf8(mat.bytes()).ok().map(str::to_string);
        self.matches.push(CollectedMatch {
            file_path: self.file_path.clone(),
            line_number,
            line_text,
        });
        if (*self.match_count as f64) >= self.effective_limit {
            *self.match_limit_reached = true;
            return Ok(false);
        }
        Ok(true)
    }
}

fn build_matcher(args: &GrepArgs) -> Result<grep_regex::RegexMatcher, ToolError> {
    let mut builder = grep_regex::RegexMatcherBuilder::new();
    builder
        .case_insensitive(args.ignore_case.unwrap_or(false))
        .case_smart(false)
        .multi_line(true)
        .fixed_strings(args.literal.unwrap_or(false))
        .line_terminator(Some(b'\n'));
    builder
        .build(&args.pattern)
        .map_err(|e| ToolError::new(e.to_string()))
}

pub fn execute(cwd: &str, args: &GrepArgs) -> ToolResultOrError<GrepToolDetails> {
    let search_dir = args.path.as_deref().unwrap_or("");
    let search_path = resolve_to_cwd(
        if search_dir.is_empty() {
            "."
        } else {
            search_dir
        },
        cwd,
    );

    let is_directory = match std::fs::metadata(&search_path) {
        Ok(meta) => meta.is_dir(),
        Err(_) => return Err(ToolError::new(format!("Path not found: {search_path}"))),
    };

    let context_value = match args.context {
        Some(c) if c > 0.0 => c as i64,
        _ => 0,
    };
    let effective_limit = args.limit.unwrap_or(DEFAULT_LIMIT).max(1.0);

    let matcher = build_matcher(args)?;
    let mut searcher = SearcherBuilder::new()
        .line_number(true)
        .binary_detection(BinaryDetection::quit(0))
        .build();

    let mut matches: Vec<CollectedMatch> = Vec::new();
    let mut match_count = 0usize;
    let mut match_limit_reached = false;
    let mut search_errors: Vec<String> = Vec::new();

    if is_directory {
        let overrides = match &args.glob {
            Some(glob) => {
                let mut ob = OverrideBuilder::new(cwd);
                ob.add(glob).map_err(|e| ToolError::new(e.to_string()))?;
                Some(ob.build().map_err(|e| ToolError::new(e.to_string()))?)
            }
            None => None,
        };

        let mut builder = WalkBuilder::new(&search_path);
        builder
            .hidden(false)
            .parents(true)
            .ignore(true)
            .git_ignore(true)
            .git_global(true)
            .git_exclude(true)
            .require_git(true)
            .follow_links(false);
        builder.add_custom_ignore_filename(".rgignore");
        if let Some(overrides) = overrides {
            builder.overrides(overrides);
        }

        'walk: for entry in builder.build() {
            let entry = match entry {
                Ok(entry) => entry,
                Err(err) => {
                    search_errors.push(err.to_string());
                    continue;
                }
            };
            if !entry.file_type().is_some_and(|ft| ft.is_file()) {
                continue;
            }
            let file_path = entry.path().to_string_lossy().into_owned();
            let sink = MatchSink {
                file_path: file_path.clone(),
                matches: &mut matches,
                match_count: &mut match_count,
                effective_limit,
                match_limit_reached: &mut match_limit_reached,
            };
            if let Err(err) = searcher.search_path(&matcher, entry.path(), sink) {
                search_errors.push(format!("{file_path}: {err}"));
            }
            if match_limit_reached {
                break 'walk;
            }
        }
    } else {
        let sink = MatchSink {
            file_path: search_path.clone(),
            matches: &mut matches,
            match_count: &mut match_count,
            effective_limit,
            match_limit_reached: &mut match_limit_reached,
        };
        if let Err(err) = searcher.search_path(&matcher, std::path::Path::new(&search_path), sink) {
            search_errors.push(format!("{search_path}: {err}"));
        }
    }

    // pi rejects with rg's stderr when rg exits with a real error and wasn't
    // killed for reaching the match limit.
    if !match_limit_reached && !search_errors.is_empty() {
        return Err(ToolError::new(search_errors.join("\n").trim().to_string()));
    }

    if match_count == 0 {
        return Ok(ToolResult::text_only("No matches found"));
    }

    let format_path = |file_path: &str| -> String {
        if is_directory {
            let relative = node_relative(&search_path, file_path);
            if !relative.is_empty() && !relative.starts_with("..") {
                return relative.replace('\\', "/");
            }
        }
        node_basename(file_path).to_string()
    };

    let mut file_cache: HashMap<String, Vec<String>> = HashMap::new();
    let mut get_file_lines = |file_path: &str| -> Vec<String> {
        if let Some(lines) = file_cache.get(file_path) {
            return lines.clone();
        }
        let lines = match std::fs::read(file_path) {
            Ok(bytes) => {
                let content = String::from_utf8_lossy(&bytes);
                content
                    .replace("\r\n", "\n")
                    .replace('\r', "\n")
                    .split('\n')
                    .map(str::to_string)
                    .collect()
            }
            Err(_) => Vec::new(),
        };
        file_cache.insert(file_path.to_string(), lines.clone());
        lines
    };

    let mut lines_truncated = false;
    let mut output_lines: Vec<String> = Vec::new();

    for m in &matches {
        if let Some(line_text) = m.line_text.as_ref().filter(|_| context_value == 0) {
            let relative_path = format_path(&m.file_path);
            let sanitized = line_text.replace("\r\n", "\n").replace('\r', "");
            let sanitized = sanitized.strip_suffix('\n').unwrap_or(&sanitized);
            let (truncated_text, was_truncated) = truncate_line(sanitized, GREP_MAX_LINE_LENGTH);
            if was_truncated {
                lines_truncated = true;
            }
            output_lines.push(format!(
                "{relative_path}:{}: {truncated_text}",
                m.line_number
            ));
        } else {
            let relative_path = format_path(&m.file_path);
            let lines = get_file_lines(&m.file_path);
            if lines.is_empty() {
                output_lines.push(format!(
                    "{relative_path}:{}: (unable to read file)",
                    m.line_number
                ));
                continue;
            }
            let line_number = m.line_number as i64;
            let start = if context_value > 0 {
                (line_number - context_value).max(1)
            } else {
                line_number
            };
            let end = if context_value > 0 {
                (line_number + context_value).min(lines.len() as i64)
            } else {
                line_number
            };
            for current in start..=end {
                let line_text = lines
                    .get((current - 1) as usize)
                    .map(String::as_str)
                    .unwrap_or("");
                let sanitized = line_text.replace('\r', "");
                let (truncated_text, was_truncated) =
                    truncate_line(&sanitized, GREP_MAX_LINE_LENGTH);
                if was_truncated {
                    lines_truncated = true;
                }
                if current == line_number {
                    output_lines.push(format!("{relative_path}:{current}: {truncated_text}"));
                } else {
                    output_lines.push(format!("{relative_path}-{current}- {truncated_text}"));
                }
            }
        }
    }

    let raw_output = output_lines.join("\n");
    // Byte truncation only; the match limit already capped rows.
    let truncation = truncate_head(
        &raw_output,
        TruncationOptions {
            max_lines: Some(usize::MAX),
            max_bytes: None,
        },
    );
    let mut output = truncation.content.clone();
    let mut details = GrepToolDetails {
        truncation: None,
        match_limit_reached: None,
        lines_truncated: None,
    };
    let mut notices: Vec<String> = Vec::new();
    if match_limit_reached {
        notices.push(format!(
            "{} matches limit reached. Use limit={} for more, or refine pattern",
            js_num(effective_limit),
            js_num(effective_limit * 2.0)
        ));
        details.match_limit_reached = Some(effective_limit as usize);
    }
    if truncation.truncated {
        notices.push(format!("{} limit reached", format_size(DEFAULT_MAX_BYTES)));
        details.truncation = Some(truncation);
    }
    if lines_truncated {
        notices.push(format!(
            "Some lines truncated to {GREP_MAX_LINE_LENGTH} chars. Use read tool to see full lines"
        ));
        details.lines_truncated = Some(true);
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
