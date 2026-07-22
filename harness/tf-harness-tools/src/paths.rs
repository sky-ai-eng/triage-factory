//! Port of pi's `utils/paths.ts` + `core/tools/path-utils.ts` (POSIX only).
//!
//! Resolution is purely lexical, like Node's `path.resolve`: `.`/`..` and
//! duplicate slashes collapse, symlinks are NOT followed, and the result never
//! has a trailing slash (except root).

use std::path::Path;

use unicode_normalization::UnicodeNormalization;

const NARROW_NO_BREAK_SPACE: char = '\u{202F}';

fn is_unicode_space(ch: char) -> bool {
    matches!(
        ch,
        '\u{00A0}' | '\u{2000}'..='\u{200A}' | '\u{202F}' | '\u{205F}' | '\u{3000}'
    )
}

pub fn home_dir() -> String {
    if let Ok(home) = std::env::var("HOME") {
        if !home.is_empty() {
            return home;
        }
    }
    // Node's os.homedir falls back to the passwd entry when HOME is unset.
    unsafe {
        let pw = libc::getpwuid(libc::getuid());
        if !pw.is_null() {
            let dir = (*pw).pw_dir;
            if !dir.is_null() {
                if let Ok(s) = std::ffi::CStr::from_ptr(dir).to_str() {
                    return s.to_string();
                }
            }
        }
    }
    String::new()
}

/// Node `path.resolve` for POSIX: process segments right-to-left until an
/// absolute one is found, then lexically normalize. Falls back to the process
/// cwd when nothing is absolute.
fn node_resolve(segments: &[&str]) -> String {
    let mut resolved = String::new();
    let mut absolute = false;
    for seg in segments.iter().rev() {
        if seg.is_empty() {
            continue;
        }
        if resolved.is_empty() {
            resolved = (*seg).to_string();
        } else {
            resolved = format!("{seg}/{resolved}");
        }
        if seg.starts_with('/') {
            absolute = true;
            break;
        }
    }
    if !absolute {
        let cwd = std::env::current_dir()
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_else(|_| "/".to_string());
        if resolved.is_empty() {
            resolved = cwd;
        } else {
            resolved = format!("{cwd}/{resolved}");
        }
    }
    normalize_lexical(&resolved)
}

fn normalize_lexical(path: &str) -> String {
    let absolute = path.starts_with('/');
    let mut stack: Vec<&str> = Vec::new();
    for part in path.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                if let Some(last) = stack.last() {
                    if *last != ".." {
                        stack.pop();
                        continue;
                    }
                }
                if !absolute {
                    stack.push("..");
                }
            }
            _ => stack.push(part),
        }
    }
    let joined = stack.join("/");
    if absolute {
        format!("/{joined}")
    } else if joined.is_empty() {
        ".".to_string()
    } else {
        joined
    }
}

fn file_url_to_path(url: &str) -> String {
    // Node's fileURLToPath, reduced to the file://[localhost]/... shapes that
    // can reach the tools; other hosts keep the raw string rather than throw.
    let rest = &url["file://".len()..];
    let (host, path) = match rest.find('/') {
        Some(idx) => (&rest[..idx], &rest[idx..]),
        None => (rest, "/"),
    };
    if !host.is_empty() && host != "localhost" {
        return url.to_string();
    }
    let mut out = Vec::with_capacity(path.len());
    let bytes = path.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let Ok(byte) = u8::from_str_radix(&path[i + 1..i + 3], 16) {
                out.push(byte);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

#[derive(Debug, Clone, Copy, Default)]
pub struct PathInputOptions {
    pub trim: bool,
    /// Defaults to true in pi; callers here pass it explicitly.
    pub expand_tilde: bool,
    pub strip_at_prefix: bool,
    pub normalize_unicode_spaces: bool,
}

impl PathInputOptions {
    pub fn tool_input() -> Self {
        PathInputOptions {
            trim: false,
            expand_tilde: true,
            strip_at_prefix: true,
            normalize_unicode_spaces: true,
        }
    }

    pub fn base_dir() -> Self {
        PathInputOptions {
            trim: false,
            expand_tilde: true,
            strip_at_prefix: false,
            normalize_unicode_spaces: false,
        }
    }
}

pub fn normalize_path(input: &str, options: PathInputOptions) -> String {
    let mut normalized: String = if options.trim {
        input.trim().to_string()
    } else {
        input.to_string()
    };
    if options.normalize_unicode_spaces {
        normalized = normalized
            .chars()
            .map(|c| if is_unicode_space(c) { ' ' } else { c })
            .collect();
    }
    if options.strip_at_prefix && normalized.starts_with('@') {
        normalized = normalized[1..].to_string();
    }

    if options.expand_tilde {
        if normalized == "~" {
            return home_dir();
        }
        if let Some(rest) = normalized.strip_prefix("~/") {
            // Node path.join(home, rest)
            let home = home_dir();
            return normalize_lexical(&format!("{home}/{rest}"));
        }
    }

    if normalized.starts_with("file://") {
        return file_url_to_path(&normalized);
    }

    normalized
}

/// pi's `resolvePath(input, baseDir, opts)`.
pub fn resolve_path(input: &str, base_dir: &str, options: PathInputOptions) -> String {
    let normalized = normalize_path(input, options);
    let normalized_base = normalize_path(base_dir, PathInputOptions::base_dir());
    if normalized.starts_with('/') {
        node_resolve(&[&normalized])
    } else {
        node_resolve(&[&normalized_base, &normalized])
    }
}

/// pi's `resolveToCwd`: tool-facing resolution with unicode-space folding,
/// `@` prefix stripping, and `~` expansion.
pub fn resolve_to_cwd(file_path: &str, cwd: &str) -> String {
    resolve_path(file_path, cwd, PathInputOptions::tool_input())
}

pub fn path_exists(path: &str) -> bool {
    // access(F_OK) follows symlinks, so a broken symlink does not exist.
    Path::new(path).metadata().is_ok()
}

fn try_macos_screenshot_path(file_path: &str) -> String {
    // JS: filePath.replace(/ (AM|PM)\./gi, " $1.") — case-insensitive,
    // preserves the matched text.
    let bytes = file_path.as_bytes();
    let mut out = String::with_capacity(file_path.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b' ' && i + 3 < bytes.len() {
            let a = bytes[i + 1].to_ascii_uppercase();
            let m = bytes[i + 2].to_ascii_uppercase();
            if (a == b'A' || a == b'P') && m == b'M' && bytes[i + 3] == b'.' {
                out.push(NARROW_NO_BREAK_SPACE);
                out.push(bytes[i + 1] as char);
                out.push(bytes[i + 2] as char);
                out.push('.');
                i += 4;
                continue;
            }
        }
        // Safe: advancing per byte only when ASCII space matched; otherwise copy
        // the full char.
        let ch_len = utf8_char_len(bytes[i]);
        out.push_str(&file_path[i..i + ch_len]);
        i += ch_len;
    }
    out
}

fn utf8_char_len(first_byte: u8) -> usize {
    match first_byte {
        b if b & 0x80 == 0 => 1,
        b if b & 0xE0 == 0xC0 => 2,
        b if b & 0xF0 == 0xE0 => 3,
        _ => 4,
    }
}

fn try_nfd_variant(file_path: &str) -> String {
    file_path.nfd().collect()
}

fn try_curly_quote_variant(file_path: &str) -> String {
    file_path.replace('\'', "\u{2019}")
}

/// pi's `resolveReadPathAsync`: try the resolved path, then macOS filename
/// variants (screenshot NBSP, NFD, curly quote, NFD+curly).
pub fn resolve_read_path(file_path: &str, cwd: &str) -> String {
    let resolved = resolve_to_cwd(file_path, cwd);
    if path_exists(&resolved) {
        return resolved;
    }
    let am_pm = try_macos_screenshot_path(&resolved);
    if am_pm != resolved && path_exists(&am_pm) {
        return am_pm;
    }
    let nfd = try_nfd_variant(&resolved);
    if nfd != resolved && path_exists(&nfd) {
        return nfd;
    }
    let curly = try_curly_quote_variant(&resolved);
    if curly != resolved && path_exists(&curly) {
        return curly;
    }
    let nfd_curly = try_curly_quote_variant(&nfd);
    if nfd_curly != resolved && path_exists(&nfd_curly) {
        return nfd_curly;
    }
    resolved
}

/// Node `path.relative` (POSIX, lexical).
pub fn node_relative(from: &str, to: &str) -> String {
    let from_res = node_resolve(&[from]);
    let to_res = node_resolve(&[to]);
    if from_res == to_res {
        return String::new();
    }
    let from_parts: Vec<&str> = from_res.split('/').filter(|s| !s.is_empty()).collect();
    let to_parts: Vec<&str> = to_res.split('/').filter(|s| !s.is_empty()).collect();
    let common = from_parts
        .iter()
        .zip(to_parts.iter())
        .take_while(|(a, b)| a == b)
        .count();
    let mut parts: Vec<String> = Vec::new();
    for _ in common..from_parts.len() {
        parts.push("..".to_string());
    }
    for part in &to_parts[common..] {
        parts.push((*part).to_string());
    }
    parts.join("/")
}

/// Node `path.basename` (POSIX).
pub fn node_basename(path: &str) -> &str {
    let trimmed = path.trim_end_matches('/');
    if trimmed.is_empty() {
        return if path.is_empty() { "" } else { "/" };
    }
    match trimmed.rfind('/') {
        Some(idx) => &trimmed[idx + 1..],
        None => trimmed,
    }
}

/// Node `path.dirname` (POSIX).
pub fn node_dirname(path: &str) -> String {
    let trimmed = path.trim_end_matches('/');
    if trimmed.is_empty() {
        return "/".to_string();
    }
    match trimmed.rfind('/') {
        Some(0) => "/".to_string(),
        Some(idx) => trimmed[..idx].to_string(),
        None => ".".to_string(),
    }
}

/// Node `path.join(a, b)` (POSIX, two-arg form).
pub fn node_join(a: &str, b: &str) -> String {
    if a.is_empty() && b.is_empty() {
        return ".".to_string();
    }
    let joined = if a.is_empty() {
        b.to_string()
    } else if b.is_empty() {
        a.to_string()
    } else {
        format!("{a}/{b}")
    };
    normalize_lexical(&joined)
}
