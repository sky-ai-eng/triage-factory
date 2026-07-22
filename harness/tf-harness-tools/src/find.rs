//! Port of pi's `core/tools/find.ts` execute path, with the `fd` subprocess
//! replaced by an in-process reimplementation of the exact invocation pi
//! uses: `fd --glob --color=never --hidden [--no-require-git] --max-results N
//! [--full-path] -- PATTERN SEARCHPATH` (verified against fd 10.2.0 source:
//! glob built with literal_separator, basename vs absolute-path candidates,
//! ignore chain incl. .fdignore, trailing slash on directory results).

use globset::GlobBuilder;
use ignore::WalkBuilder;
use serde::Deserialize;

use crate::jsnum::js_num;
use crate::jsstring::is_js_whitespace;
use crate::paths::{node_dirname, node_join, node_relative, path_exists, resolve_to_cwd};
use crate::result::{
    ContentBlock, EmptyCheck, FindToolDetails, ToolError, ToolResult, ToolResultOrError,
};
use crate::truncate::{format_size, truncate_head, TruncationOptions, DEFAULT_MAX_BYTES};

pub const DEFAULT_LIMIT: f64 = 1000.0;

#[derive(Debug, Clone, Deserialize)]
pub struct FindArgs {
    pub pattern: String,
    #[serde(default)]
    pub path: Option<String>,
    #[serde(default)]
    pub limit: Option<f64>,
}

fn to_posix(value: &str) -> String {
    value.to_string()
}

pub fn execute(cwd: &str, args: &FindArgs) -> ToolResultOrError<FindToolDetails> {
    let search_dir = args.path.as_deref().unwrap_or("");
    let search_path = resolve_to_cwd(
        if search_dir.is_empty() {
            "."
        } else {
            search_dir
        },
        cwd,
    );
    let effective_limit = args.limit.unwrap_or(DEFAULT_LIMIT);

    if !path_exists(&search_path) {
        return Err(ToolError::new(format!("Path not found: {search_path}")));
    }

    // fd only respects .gitignore outside git repos with --no-require-git;
    // inside repos, default git-aware behavior stops parent rules at nested
    // repo boundaries.
    let mut inside_git_repo = false;
    let mut current = search_path.clone();
    loop {
        if path_exists(&node_join(&current, ".git")) {
            inside_git_repo = true;
            break;
        }
        let parent = node_dirname(&current);
        if parent == current {
            break;
        }
        current = parent;
    }

    // fd --glob matches the basename unless the pattern contains a path
    // separator; then --full-path matches the absolute candidate path, which
    // needs a leading **/ to match anything.
    let mut effective_pattern = args.pattern.clone();
    let full_path = args.pattern.contains('/');
    if full_path
        && !args.pattern.starts_with('/')
        && !args.pattern.starts_with("**/")
        && args.pattern != "**"
    {
        effective_pattern = format!("**/{}", args.pattern);
    }

    // The bare globset message ("error parsing glob '...': ...") without
    // fd's "[fd error]:" stderr framing — no fd binary exists here, and
    // agent-facing text stays unbranded or TF-branded.
    let glob = GlobBuilder::new(&effective_pattern)
        .literal_separator(true)
        .build()
        .map_err(|e| ToolError::new(e.to_string()))?;
    let matcher = glob.compile_matcher();

    let mut builder = WalkBuilder::new(&search_path);
    builder
        .hidden(false)
        .ignore(true)
        .parents(true)
        .git_ignore(true)
        .git_global(true)
        .git_exclude(true)
        .require_git(inside_git_repo)
        .follow_links(false);
    builder.add_custom_ignore_filename(".fdignore");

    struct RawResult {
        line: String,
    }

    let mut raw_results: Vec<RawResult> = Vec::new();
    for entry in builder.build() {
        let Ok(entry) = entry else {
            // fd prints walk errors to stderr and continues.
            continue;
        };
        if entry.depth() == 0 {
            // The ignore walker yields the search root; fd's parallel walker
            // does not report it as a candidate.
            continue;
        }
        let path = entry.path();
        let matched = if full_path {
            matcher.is_match(path)
        } else {
            match path.file_name() {
                Some(name) => matcher.is_match(std::path::Path::new(name)),
                None => false,
            }
        };
        if !matched {
            continue;
        }
        let is_dir = entry.file_type().is_some_and(|ft| ft.is_dir());
        let mut line = path.to_string_lossy().into_owned();
        if is_dir {
            line.push('/');
        }
        raw_results.push(RawResult { line });
        if raw_results.len() as f64 >= effective_limit {
            break;
        }
    }

    if raw_results.is_empty() {
        return Ok(ToolResult::text_only("No files found matching pattern"));
    }

    // pi's relativization of fd's stdout lines.
    let mut relativized: Vec<String> = Vec::new();
    for raw in &raw_results {
        let line = raw.line.strip_suffix('\r').unwrap_or(&raw.line);
        let line = line.trim_matches(is_js_whitespace);
        if line.is_empty() {
            continue;
        }
        let had_trailing_slash = line.ends_with('/') || line.ends_with('\\');
        let mut relative_path = if let Some(rest) = line.strip_prefix(&search_path) {
            rest.get(1..).unwrap_or("").to_string()
        } else {
            node_relative(&search_path, line)
        };
        if had_trailing_slash && !relative_path.ends_with('/') {
            relative_path.push('/');
        }
        relativized.push(to_posix(&relative_path));
    }

    let result_limit_reached = relativized.len() as f64 >= effective_limit;
    let raw_output = relativized.join("\n");
    let truncation = truncate_head(
        &raw_output,
        TruncationOptions {
            max_lines: Some(usize::MAX),
            max_bytes: None,
        },
    );
    let mut result_output = truncation.content.clone();
    let mut details = FindToolDetails {
        truncation: None,
        result_limit_reached: None,
    };
    let mut notices: Vec<String> = Vec::new();
    if result_limit_reached {
        notices.push(format!(
            "{} results limit reached. Use limit={} for more, or refine pattern",
            js_num(effective_limit),
            js_num(effective_limit * 2.0)
        ));
        details.result_limit_reached = Some(effective_limit as usize);
    }
    if truncation.truncated {
        notices.push(format!("{} limit reached", format_size(DEFAULT_MAX_BYTES)));
        details.truncation = Some(truncation);
    }
    if !notices.is_empty() {
        result_output.push_str(&format!("\n\n[{}]", notices.join(". ")));
    }

    Ok(ToolResult {
        content: vec![ContentBlock::text(result_output)],
        details: if details.is_empty_details() {
            None
        } else {
            Some(details)
        },
    })
}
