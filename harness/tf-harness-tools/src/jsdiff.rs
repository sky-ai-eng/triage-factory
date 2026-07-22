//! Faithful port of jsdiff 8.0.4's line diff (`diff/base.js` + `diff/line.js`)
//! and unified-patch generation (`patch/create.js` with FILE_HEADERS_ONLY),
//! the exact library pi uses for the edit tool's diff/patch details. The
//! greedy forward Myers walk is ported as-is so tie-breaking (and therefore
//! the emitted diff) is byte-identical.

#[derive(Debug, Clone)]
pub struct Part {
    pub value: String,
    pub count: usize,
    pub added: bool,
    pub removed: bool,
}

#[derive(Debug, Clone, Copy)]
struct Component {
    count: usize,
    added: bool,
    removed: bool,
    prev: Option<usize>,
}

#[derive(Debug, Clone, Copy)]
struct Path {
    old_pos: isize,
    last_component: Option<usize>,
}

/// jsdiff line tokenizer: lines keep their trailing newline; a final segment
/// without a newline is its own token; empty tokens are dropped.
fn tokenize_lines(value: &str) -> Vec<&str> {
    let mut tokens: Vec<&str> = Vec::new();
    let bytes = value.as_bytes();
    let mut line_start = 0usize;
    let mut i = 0usize;
    while i < bytes.len() {
        if bytes[i] == b'\n' {
            tokens.push(&value[line_start..=i]);
            line_start = i + 1;
            i += 1;
        } else if bytes[i] == b'\r' && i + 1 < bytes.len() && bytes[i + 1] == b'\n' {
            tokens.push(&value[line_start..i + 2]);
            line_start = i + 2;
            i += 2;
        } else {
            i += 1;
        }
    }
    if line_start < bytes.len() {
        tokens.push(&value[line_start..]);
    }
    tokens.retain(|t| !t.is_empty());
    tokens
}

struct DiffState<'a> {
    old_tokens: Vec<&'a str>,
    new_tokens: Vec<&'a str>,
    arena: Vec<Component>,
}

impl<'a> DiffState<'a> {
    fn push_component(
        &mut self,
        count: usize,
        added: bool,
        removed: bool,
        prev: Option<usize>,
    ) -> usize {
        self.arena.push(Component {
            count,
            added,
            removed,
            prev,
        });
        self.arena.len() - 1
    }

    fn add_to_path(&mut self, path: Path, added: bool, removed: bool, old_pos_inc: isize) -> Path {
        let last = path.last_component.map(|i| self.arena[i]);
        match last {
            Some(l) if l.added == added && l.removed == removed => {
                let idx = self.push_component(l.count + 1, added, removed, l.prev);
                Path {
                    old_pos: path.old_pos + old_pos_inc,
                    last_component: Some(idx),
                }
            }
            _ => {
                let idx = self.push_component(1, added, removed, path.last_component);
                Path {
                    old_pos: path.old_pos + old_pos_inc,
                    last_component: Some(idx),
                }
            }
        }
    }

    fn extract_common(&mut self, base_path: &mut Path, diagonal_path: isize) -> isize {
        let new_len = self.new_tokens.len() as isize;
        let old_len = self.old_tokens.len() as isize;
        let mut old_pos = base_path.old_pos;
        let mut new_pos = old_pos - diagonal_path;
        let mut common_count = 0usize;
        while new_pos + 1 < new_len
            && old_pos + 1 < old_len
            && self.old_tokens[(old_pos + 1) as usize] == self.new_tokens[(new_pos + 1) as usize]
        {
            new_pos += 1;
            old_pos += 1;
            common_count += 1;
        }
        if common_count > 0 {
            let idx = self.push_component(common_count, false, false, base_path.last_component);
            base_path.last_component = Some(idx);
        }
        base_path.old_pos = old_pos;
        new_pos
    }

    fn build_values(&self, last_component: Option<usize>) -> Vec<Part> {
        let mut components: Vec<Component> = Vec::new();
        let mut cursor = last_component;
        while let Some(idx) = cursor {
            components.push(self.arena[idx]);
            cursor = self.arena[idx].prev;
        }
        components.reverse();

        let mut parts: Vec<Part> = Vec::with_capacity(components.len());
        let mut new_pos = 0usize;
        let mut old_pos = 0usize;
        for component in &components {
            if !component.removed {
                let value: String = self.new_tokens[new_pos..new_pos + component.count].concat();
                new_pos += component.count;
                if !component.added {
                    old_pos += component.count;
                }
                parts.push(Part {
                    value,
                    count: component.count,
                    added: component.added,
                    removed: false,
                });
            } else {
                let value: String = self.old_tokens[old_pos..old_pos + component.count].concat();
                old_pos += component.count;
                parts.push(Part {
                    value,
                    count: component.count,
                    added: false,
                    removed: true,
                });
            }
        }
        parts
    }
}

/// jsdiff `diffLines(oldStr, newStr)` with default options.
pub fn diff_lines(old_str: &str, new_str: &str) -> Vec<Part> {
    let mut state = DiffState {
        old_tokens: tokenize_lines(old_str),
        new_tokens: tokenize_lines(new_str),
        arena: Vec::new(),
    };
    let new_len = state.new_tokens.len() as isize;
    let old_len = state.old_tokens.len() as isize;
    let max_edit_length = new_len + old_len;

    // bestPath indexed by diagonal; JS arrays tolerate reads one past the
    // range, so size for [-max-1, +max+1].
    let offset = max_edit_length + 1;
    let mut best_path: Vec<Option<Path>> = vec![None; (2 * max_edit_length + 3).max(3) as usize];
    let diag = |d: isize| (d + offset) as usize;

    let mut seed = Path {
        old_pos: -1,
        last_component: None,
    };
    let new_pos_seed = state.extract_common(&mut seed, 0);
    if seed.old_pos + 1 >= old_len && new_pos_seed + 1 >= new_len {
        return state.build_values(seed.last_component);
    }
    best_path[diag(0)] = Some(seed);

    let mut min_diagonal_to_consider = isize::MIN;
    let mut max_diagonal_to_consider = isize::MAX;
    let mut edit_length: isize = 1;

    while edit_length <= max_edit_length {
        let mut diagonal_path = min_diagonal_to_consider.max(-edit_length);
        let upper = max_diagonal_to_consider.min(edit_length);
        while diagonal_path <= upper {
            let remove_path = best_path[diag(diagonal_path - 1)];
            let add_path = best_path[diag(diagonal_path + 1)];
            if remove_path.is_some() {
                best_path[diag(diagonal_path - 1)] = None;
            }

            let mut can_add = false;
            if let Some(ap) = add_path {
                let add_path_new_pos = ap.old_pos - diagonal_path;
                can_add = 0 <= add_path_new_pos && add_path_new_pos < new_len;
            }
            let can_remove = remove_path.is_some_and(|rp| rp.old_pos + 1 < old_len);
            if !can_add && !can_remove {
                best_path[diag(diagonal_path)] = None;
                diagonal_path += 2;
                continue;
            }

            // Branch from the prior path whose old-string position is
            // farthest from the origin, within graph bounds.
            let mut base_path = if !can_remove
                || (can_add && remove_path.unwrap().old_pos < add_path.unwrap().old_pos)
            {
                state.add_to_path(add_path.unwrap(), true, false, 0)
            } else {
                state.add_to_path(remove_path.unwrap(), false, true, 1)
            };

            let new_pos = state.extract_common(&mut base_path, diagonal_path);
            if base_path.old_pos + 1 >= old_len && new_pos + 1 >= new_len {
                return state.build_values(base_path.last_component);
            }
            if base_path.old_pos + 1 >= old_len {
                max_diagonal_to_consider = max_diagonal_to_consider.min(diagonal_path - 1);
            }
            if new_pos + 1 >= new_len {
                min_diagonal_to_consider = min_diagonal_to_consider.max(diagonal_path + 1);
            }
            best_path[diag(diagonal_path)] = Some(base_path);

            diagonal_path += 2;
        }
        edit_length += 1;
    }

    // Unreachable with no maxEditLength cap; return an empty diff defensively.
    Vec::new()
}

/// `patch/create.js` splitLines: lines including their trailing newline; the
/// final line keeps its no-newline state.
fn split_lines_keep_nl(text: &str) -> Vec<String> {
    let has_trailing_nl = text.ends_with('\n');
    let mut result: Vec<String> = text.split('\n').map(|l| format!("{l}\n")).collect();
    if has_trailing_nl {
        result.pop();
    } else if let Some(last) = result.pop() {
        result.push(last[..last.len() - 1].to_string());
    }
    result
}

struct Hunk {
    old_start: usize,
    old_lines: usize,
    new_start: usize,
    new_lines: usize,
    lines: Vec<String>,
}

/// jsdiff `createTwoFilesPatch(path, path, old, new, undefined, undefined,
/// {context, headerOptions: FILE_HEADERS_ONLY})`.
pub fn create_two_files_patch(
    old_file_name: &str,
    new_file_name: &str,
    old_str: &str,
    new_str: &str,
    context: usize,
) -> String {
    let mut diff = diff_lines(old_str, new_str);
    // Sentinel eases cleanup, as in jsdiff.
    diff.push(Part {
        value: String::new(),
        count: 0,
        added: false,
        removed: false,
    });

    let context_lines =
        |lines: &[String]| -> Vec<String> { lines.iter().map(|l| format!(" {l}")).collect() };

    let mut hunks: Vec<Hunk> = Vec::new();
    let mut old_range_start = 0usize;
    let mut new_range_start = 0usize;
    let mut cur_range: Vec<String> = Vec::new();
    let mut old_line = 1usize;
    let mut new_line = 1usize;

    // The sentinel carries an explicit empty `lines: []` in jsdiff; running
    // its empty value through splitLines would fabricate one empty line.
    let split: Vec<Vec<String>> = diff
        .iter()
        .enumerate()
        .map(|(i, p)| {
            if i == diff.len() - 1 {
                Vec::new()
            } else {
                split_lines_keep_nl(&p.value)
            }
        })
        .collect();

    for i in 0..diff.len() {
        let current = &diff[i];
        let lines = &split[i];
        if current.added || current.removed {
            if old_range_start == 0 {
                old_range_start = old_line;
                new_range_start = new_line;
                if i > 0 {
                    let prev_lines = &split[i - 1];
                    cur_range = if context > 0 {
                        let take = prev_lines.len().min(context);
                        context_lines(&prev_lines[prev_lines.len() - take..])
                    } else {
                        Vec::new()
                    };
                    old_range_start -= cur_range.len();
                    new_range_start -= cur_range.len();
                }
            }
            for line in lines {
                cur_range.push(format!("{}{line}", if current.added { '+' } else { '-' }));
            }
            if current.added {
                new_line += lines.len();
            } else {
                old_line += lines.len();
            }
        } else {
            if old_range_start != 0 {
                if lines.len() <= context * 2 && i < diff.len() - 2 {
                    // Overlapping context joins the ranges.
                    cur_range.extend(context_lines(lines));
                } else {
                    let context_size = lines.len().min(context);
                    cur_range.extend(context_lines(&lines[..context_size]));
                    hunks.push(Hunk {
                        old_start: old_range_start,
                        old_lines: old_line - old_range_start + context_size,
                        new_start: new_range_start,
                        new_lines: new_line - new_range_start + context_size,
                        lines: std::mem::take(&mut cur_range),
                    });
                    old_range_start = 0;
                    new_range_start = 0;
                }
            }
            old_line += lines.len();
            new_line += lines.len();
        }
    }

    // Strip the trailing newline from each hunk line, adding the no-newline
    // marker where a line had none.
    for hunk in &mut hunks {
        let mut i = 0;
        while i < hunk.lines.len() {
            if hunk.lines[i].ends_with('\n') {
                let trimmed = hunk.lines[i][..hunk.lines[i].len() - 1].to_string();
                hunk.lines[i] = trimmed;
            } else {
                hunk.lines
                    .insert(i + 1, "\\ No newline at end of file".to_string());
                i += 1;
            }
            i += 1;
        }
    }

    // formatPatch with FILE_HEADERS_ONLY.
    let mut ret: Vec<String> = Vec::new();
    ret.push(format!("--- {old_file_name}"));
    ret.push(format!("+++ {new_file_name}"));
    for hunk in &mut hunks {
        let mut old_start = hunk.old_start as isize;
        let mut new_start = hunk.new_start as isize;
        // Unified diff quirk: zero-length ranges report one line earlier.
        if hunk.old_lines == 0 {
            old_start -= 1;
        }
        if hunk.new_lines == 0 {
            new_start -= 1;
        }
        ret.push(format!(
            "@@ -{},{} +{},{} @@",
            old_start, hunk.old_lines, new_start, hunk.new_lines
        ));
        ret.extend(hunk.lines.iter().cloned());
    }
    let mut out = ret.join("\n");
    out.push('\n');
    out
}
