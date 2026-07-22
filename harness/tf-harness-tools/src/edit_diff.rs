//! Port of pi's `core/tools/edit-diff.ts`: line-ending handling, fuzzy
//! matching, multi-edit application, and the display diff. All offsets are
//! byte offsets into UTF-8 strings (pi uses UTF-16 offsets; both are
//! internally consistent, and every observable output is a string).

use unicode_normalization::UnicodeNormalization;

use crate::jsdiff;
use crate::jsstring::js_trim_end;
use crate::result::ToolError;

pub fn detect_line_ending(content: &str) -> &'static str {
    let crlf_idx = content.find("\r\n");
    let lf_idx = content.find('\n');
    match (lf_idx, crlf_idx) {
        (None, _) => "\n",
        (_, None) => "\n",
        (Some(lf), Some(crlf)) => {
            if crlf < lf {
                "\r\n"
            } else {
                "\n"
            }
        }
    }
}

pub fn normalize_to_lf(text: &str) -> String {
    text.replace("\r\n", "\n").replace('\r', "\n")
}

pub fn restore_line_endings(text: &str, ending: &str) -> String {
    if ending == "\r\n" {
        text.replace('\n', "\r\n")
    } else {
        text.to_string()
    }
}

/// Normalize text for fuzzy matching: NFKC, strip trailing whitespace per
/// line, fold smart quotes / Unicode dashes / special spaces to ASCII.
pub fn normalize_for_fuzzy_match(text: &str) -> String {
    let nfkc: String = text.nfkc().collect();
    let trimmed: String = nfkc
        .split('\n')
        .map(js_trim_end)
        .collect::<Vec<_>>()
        .join("\n");
    trimmed
        .chars()
        .map(|c| match c {
            '\u{2018}' | '\u{2019}' | '\u{201A}' | '\u{201B}' => '\'',
            '\u{201C}' | '\u{201D}' | '\u{201E}' | '\u{201F}' => '"',
            '\u{2010}' | '\u{2011}' | '\u{2012}' | '\u{2013}' | '\u{2014}' | '\u{2015}'
            | '\u{2212}' => '-',
            '\u{00A0}' | '\u{2002}'..='\u{200A}' | '\u{202F}' | '\u{205F}' | '\u{3000}' => ' ',
            other => other,
        })
        .collect()
}

/// `content.match(/[^\n]*\n|[^\n]+/g) ?? []`: lines including their trailing
/// newline; a final line without a newline is kept; empty content is empty.
fn split_lines_with_endings(content: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let bytes = content.as_bytes();
    let mut start = 0usize;
    for (i, b) in bytes.iter().enumerate() {
        if *b == b'\n' {
            out.push(&content[start..=i]);
            start = i + 1;
        }
    }
    if start < bytes.len() {
        out.push(&content[start..]);
    }
    out
}

#[derive(Debug, Clone, Copy)]
struct LineSpan {
    start: usize,
    end: usize,
}

#[derive(Debug, Clone)]
pub struct MatchedEdit {
    pub edit_index: usize,
    pub match_index: usize,
    pub match_length: usize,
    pub new_text: String,
}

fn get_line_spans(content: &str) -> Vec<LineSpan> {
    let mut offset = 0usize;
    split_lines_with_endings(content)
        .iter()
        .map(|line| {
            let span = LineSpan {
                start: offset,
                end: offset + line.len(),
            };
            offset = span.end;
            span
        })
        .collect()
}

fn get_replacement_line_range(
    lines: &[LineSpan],
    replacement: &MatchedEdit,
) -> Result<(usize, usize), ToolError> {
    let replacement_start = replacement.match_index;
    let replacement_end = replacement.match_index + replacement.match_length;

    let mut start_line: Option<usize> = None;
    for (i, line) in lines.iter().enumerate() {
        if replacement_start >= line.start && replacement_start < line.end {
            start_line = Some(i);
            break;
        }
    }
    let Some(start_line) = start_line else {
        return Err(ToolError::new(
            "Replacement range is outside the base content.",
        ));
    };

    let mut end_line = start_line;
    while end_line < lines.len() && lines[end_line].end < replacement_end {
        end_line += 1;
    }
    if end_line >= lines.len() {
        return Err(ToolError::new(
            "Replacement range is outside the base content.",
        ));
    }

    Ok((start_line, end_line + 1))
}

fn apply_replacements(content: &str, replacements: &[MatchedEdit], offset: usize) -> String {
    let mut result = content.to_string();
    for replacement in replacements.iter().rev() {
        let match_index = replacement.match_index - offset;
        result = format!(
            "{}{}{}",
            &result[..match_index],
            replacement.new_text,
            &result[match_index + replacement.match_length..]
        );
    }
    result
}

/// Apply replacements matched against normalized `base_content` onto
/// `original_content`, copying untouched lines from the original so
/// normalization only affects the lines an edit actually rewrites.
pub fn apply_replacements_preserving_unchanged_lines(
    original_content: &str,
    base_content: &str,
    replacements: &[MatchedEdit],
) -> Result<String, ToolError> {
    let original_lines = split_lines_with_endings(original_content);
    let base_lines = get_line_spans(base_content);
    if original_lines.len() != base_lines.len() {
        return Err(ToolError::new(
            "Cannot preserve unchanged lines because the base content has a different line count.",
        ));
    }

    struct Group {
        start_line: usize,
        end_line: usize,
        replacements: Vec<MatchedEdit>,
    }

    let mut sorted: Vec<MatchedEdit> = replacements.to_vec();
    sorted.sort_by_key(|r| r.match_index);
    let mut groups: Vec<Group> = Vec::new();
    for replacement in sorted {
        let (start_line, end_line) = get_replacement_line_range(&base_lines, &replacement)?;
        if let Some(current) = groups.last_mut() {
            if start_line < current.end_line {
                current.end_line = current.end_line.max(end_line);
                current.replacements.push(replacement);
                continue;
            }
        }
        groups.push(Group {
            start_line,
            end_line,
            replacements: vec![replacement],
        });
    }

    let mut original_line_index = 0usize;
    let mut result = String::new();
    for group in &groups {
        result.push_str(&original_lines[original_line_index..group.start_line].concat());
        let group_start_offset = base_lines[group.start_line].start;
        let group_end_offset = base_lines[group.end_line - 1].end;
        result.push_str(&apply_replacements(
            &base_content[group_start_offset..group_end_offset],
            &group.replacements,
            group_start_offset,
        ));
        original_line_index = group.end_line;
    }
    result.push_str(&original_lines[original_line_index..].concat());

    Ok(result)
}

pub struct FuzzyMatchResult {
    pub found: bool,
    pub index: usize,
    pub match_length: usize,
    pub used_fuzzy_match: bool,
}

#[derive(Debug, Clone)]
pub struct Edit {
    pub old_text: String,
    pub new_text: String,
}

pub struct AppliedEditsResult {
    pub base_content: String,
    pub new_content: String,
}

/// Find `old_text` in `content`: exact match first, then fuzzy (offsets in
/// fuzzy-normalized space when fuzzy).
pub fn fuzzy_find_text(content: &str, old_text: &str) -> FuzzyMatchResult {
    if let Some(exact_index) = content.find(old_text) {
        return FuzzyMatchResult {
            found: true,
            index: exact_index,
            match_length: old_text.len(),
            used_fuzzy_match: false,
        };
    }

    let fuzzy_content = normalize_for_fuzzy_match(content);
    let fuzzy_old_text = normalize_for_fuzzy_match(old_text);
    match fuzzy_content.find(&fuzzy_old_text) {
        None => FuzzyMatchResult {
            found: false,
            index: 0,
            match_length: 0,
            used_fuzzy_match: false,
        },
        Some(fuzzy_index) => FuzzyMatchResult {
            found: true,
            index: fuzzy_index,
            match_length: fuzzy_old_text.len(),
            used_fuzzy_match: true,
        },
    }
}

/// Strip a UTF-8 BOM, returning (bom, text).
pub fn strip_bom(content: &str) -> (&'static str, &str) {
    match content.strip_prefix('\u{FEFF}') {
        Some(rest) => ("\u{FEFF}", rest),
        None => ("", content),
    }
}

fn count_occurrences(content: &str, old_text: &str) -> usize {
    let fuzzy_content = normalize_for_fuzzy_match(content);
    let fuzzy_old_text = normalize_for_fuzzy_match(old_text);
    if fuzzy_old_text.is_empty() {
        return 0;
    }
    fuzzy_content.matches(&fuzzy_old_text).count()
}

fn not_found_error(path: &str, edit_index: usize, total_edits: usize) -> ToolError {
    if total_edits == 1 {
        ToolError::new(format!(
            "Could not find the exact text in {path}. The old text must match exactly including all whitespace and newlines."
        ))
    } else {
        ToolError::new(format!(
            "Could not find edits[{edit_index}] in {path}. The oldText must match exactly including all whitespace and newlines."
        ))
    }
}

fn duplicate_error(
    path: &str,
    edit_index: usize,
    total_edits: usize,
    occurrences: usize,
) -> ToolError {
    if total_edits == 1 {
        ToolError::new(format!(
            "Found {occurrences} occurrences of the text in {path}. The text must be unique. Please provide more context to make it unique."
        ))
    } else {
        ToolError::new(format!(
            "Found {occurrences} occurrences of edits[{edit_index}] in {path}. Each oldText must be unique. Please provide more context to make it unique."
        ))
    }
}

fn empty_old_text_error(path: &str, edit_index: usize, total_edits: usize) -> ToolError {
    if total_edits == 1 {
        ToolError::new(format!("oldText must not be empty in {path}."))
    } else {
        ToolError::new(format!(
            "edits[{edit_index}].oldText must not be empty in {path}."
        ))
    }
}

fn no_change_error(path: &str, total_edits: usize) -> ToolError {
    if total_edits == 1 {
        ToolError::new(format!(
            "No changes made to {path}. The replacement produced identical content. This might indicate an issue with special characters or the text not existing as expected."
        ))
    } else {
        ToolError::new(format!(
            "No changes made to {path}. The replacements produced identical content."
        ))
    }
}

/// Apply one or more exact-text replacements to LF-normalized content. All
/// edits match against the same original content; replacements apply in
/// reverse offset order. If any edit needed fuzzy matching, the whole
/// operation runs in fuzzy-normalized space and overlays line-level changes
/// back onto the original.
pub fn apply_edits_to_normalized_content(
    normalized_content: &str,
    edits: &[Edit],
    path: &str,
) -> Result<AppliedEditsResult, ToolError> {
    let normalized_edits: Vec<Edit> = edits
        .iter()
        .map(|e| Edit {
            old_text: normalize_to_lf(&e.old_text),
            new_text: normalize_to_lf(&e.new_text),
        })
        .collect();

    for (i, edit) in normalized_edits.iter().enumerate() {
        if edit.old_text.is_empty() {
            return Err(empty_old_text_error(path, i, normalized_edits.len()));
        }
    }

    let initial_matches: Vec<FuzzyMatchResult> = normalized_edits
        .iter()
        .map(|e| fuzzy_find_text(normalized_content, &e.old_text))
        .collect();
    let used_fuzzy_match = initial_matches.iter().any(|m| m.used_fuzzy_match);
    let replacement_base_content = if used_fuzzy_match {
        normalize_for_fuzzy_match(normalized_content)
    } else {
        normalized_content.to_string()
    };

    let mut matched_edits: Vec<MatchedEdit> = Vec::new();
    for (i, edit) in normalized_edits.iter().enumerate() {
        let match_result = fuzzy_find_text(&replacement_base_content, &edit.old_text);
        if !match_result.found {
            return Err(not_found_error(path, i, normalized_edits.len()));
        }

        let occurrences = count_occurrences(&replacement_base_content, &edit.old_text);
        if occurrences > 1 {
            return Err(duplicate_error(
                path,
                i,
                normalized_edits.len(),
                occurrences,
            ));
        }

        matched_edits.push(MatchedEdit {
            edit_index: i,
            match_index: match_result.index,
            match_length: match_result.match_length,
            new_text: edit.new_text.clone(),
        });
    }

    matched_edits.sort_by_key(|m| m.match_index);
    for i in 1..matched_edits.len() {
        let previous = &matched_edits[i - 1];
        let current = &matched_edits[i];
        if previous.match_index + previous.match_length > current.match_index {
            return Err(ToolError::new(format!(
                "edits[{}] and edits[{}] overlap in {path}. Merge them into one edit or target disjoint regions.",
                previous.edit_index, current.edit_index
            )));
        }
    }

    let base_content = normalized_content.to_string();
    let new_content = if used_fuzzy_match {
        apply_replacements_preserving_unchanged_lines(
            normalized_content,
            &replacement_base_content,
            &matched_edits,
        )?
    } else {
        apply_replacements(&replacement_base_content, &matched_edits, 0)
    };

    if base_content == new_content {
        return Err(no_change_error(path, normalized_edits.len()));
    }

    Ok(AppliedEditsResult {
        base_content,
        new_content,
    })
}

/// pi's `generateUnifiedPatch`.
pub fn generate_unified_patch(path: &str, old_content: &str, new_content: &str) -> String {
    jsdiff::create_two_files_patch(path, path, old_content, new_content, 4)
}

pub struct DiffStringResult {
    pub diff: String,
    pub first_changed_line: Option<usize>,
}

/// pi's `generateDiffString`: display diff with line numbers and elided
/// context.
pub fn generate_diff_string(old_content: &str, new_content: &str) -> DiffStringResult {
    let context_lines = 4usize;
    let parts = jsdiff::diff_lines(old_content, new_content);
    let mut output: Vec<String> = Vec::new();

    let old_line_count = old_content.split('\n').count();
    let new_line_count = new_content.split('\n').count();
    let max_line_num = old_line_count.max(new_line_count);
    let line_num_width = max_line_num.to_string().len();

    let mut old_line_num = 1usize;
    let mut new_line_num = 1usize;
    let mut last_was_change = false;
    let mut first_changed_line: Option<usize> = None;

    let pad = |n: usize| format!("{:>width$}", n, width = line_num_width);
    let blank_pad = " ".repeat(line_num_width);

    for i in 0..parts.len() {
        let part = &parts[i];
        let mut raw: Vec<&str> = part.value.split('\n').collect();
        if raw.last() == Some(&"") {
            raw.pop();
        }

        if part.added || part.removed {
            if first_changed_line.is_none() {
                first_changed_line = Some(new_line_num);
            }
            for line in &raw {
                if part.added {
                    output.push(format!("+{} {line}", pad(new_line_num)));
                    new_line_num += 1;
                } else {
                    output.push(format!("-{} {line}", pad(old_line_num)));
                    old_line_num += 1;
                }
            }
            last_was_change = true;
        } else {
            let next_part_is_change =
                i < parts.len() - 1 && (parts[i + 1].added || parts[i + 1].removed);
            let has_leading_change = last_was_change;
            let has_trailing_change = next_part_is_change;

            if has_leading_change && has_trailing_change {
                if raw.len() <= context_lines * 2 {
                    for line in &raw {
                        output.push(format!(" {} {line}", pad(old_line_num)));
                        old_line_num += 1;
                        new_line_num += 1;
                    }
                } else {
                    let leading = &raw[..context_lines];
                    let trailing = &raw[raw.len() - context_lines..];
                    let skipped = raw.len() - leading.len() - trailing.len();
                    for line in leading {
                        output.push(format!(" {} {line}", pad(old_line_num)));
                        old_line_num += 1;
                        new_line_num += 1;
                    }
                    output.push(format!(" {blank_pad} ..."));
                    old_line_num += skipped;
                    new_line_num += skipped;
                    for line in trailing {
                        output.push(format!(" {} {line}", pad(old_line_num)));
                        old_line_num += 1;
                        new_line_num += 1;
                    }
                }
            } else if has_leading_change {
                let shown = &raw[..raw.len().min(context_lines)];
                let skipped = raw.len() - shown.len();
                for line in shown {
                    output.push(format!(" {} {line}", pad(old_line_num)));
                    old_line_num += 1;
                    new_line_num += 1;
                }
                if skipped > 0 {
                    output.push(format!(" {blank_pad} ..."));
                    old_line_num += skipped;
                    new_line_num += skipped;
                }
            } else if has_trailing_change {
                let skipped = raw.len().saturating_sub(context_lines);
                if skipped > 0 {
                    output.push(format!(" {blank_pad} ..."));
                    old_line_num += skipped;
                    new_line_num += skipped;
                }
                for line in &raw[skipped..] {
                    output.push(format!(" {} {line}", pad(old_line_num)));
                    old_line_num += 1;
                    new_line_num += 1;
                }
            } else {
                // Context far from any change is skipped entirely.
                old_line_num += raw.len();
                new_line_num += raw.len();
            }

            last_was_change = false;
        }
    }

    DiffStringResult {
        diff: output.join("\n"),
        first_changed_line,
    }
}
