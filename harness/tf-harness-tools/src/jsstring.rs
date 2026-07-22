//! Helpers that reproduce JavaScript string semantics on Rust strings.
//!
//! The pi tools are specified in terms of JS strings (UTF-16 code units,
//! `String.prototype.trimEnd`'s whitespace set, lossy UTF-8 decoding via
//! `Buffer.toString("utf-8")`). Byte-for-byte output parity requires matching
//! those semantics, not Rust's defaults.

/// String length in UTF-16 code units (JS `string.length`).
pub fn utf16_len(s: &str) -> usize {
    s.chars().map(char::len_utf16).sum()
}

/// Byte offset corresponding to the first `n` UTF-16 code units.
///
/// If `n` lands in the middle of a surrogate pair (an astral-plane char), the
/// char is excluded — the only divergence from JS `slice`, which would emit a
/// lone surrogate there.
pub fn utf16_slice_to(s: &str, n: usize) -> &str {
    let mut units = 0usize;
    for (byte_idx, ch) in s.char_indices() {
        let w = ch.len_utf16();
        if units + w > n {
            return &s[..byte_idx];
        }
        units += w;
    }
    s
}

/// Whether a char is in JS `trimEnd`'s trim set: WhiteSpace (TAB, VT, FF,
/// SP, NBSP, ZWNBSP, category Zs) plus LineTerminator (LF, CR, LS, PS).
/// Deliberately not `char::is_whitespace`: JS includes U+FEFF and excludes
/// U+0085.
pub fn is_js_whitespace(ch: char) -> bool {
    matches!(
        ch,
        '\u{0009}'
            | '\u{000A}'
            | '\u{000B}'
            | '\u{000C}'
            | '\u{000D}'
            | '\u{0020}'
            | '\u{00A0}'
            | '\u{1680}'
            | '\u{2000}'
            ..='\u{200A}'
                | '\u{2028}'
                | '\u{2029}'
                | '\u{202F}'
                | '\u{205F}'
                | '\u{3000}'
                | '\u{FEFF}'
    )
}

/// JS `String.prototype.trimEnd`.
pub fn js_trim_end(s: &str) -> &str {
    s.trim_end_matches(is_js_whitespace)
}

/// Split on `\n` exactly like JS `content.split("\n")`: always at least one
/// element, trailing newline yields a trailing empty element.
pub fn split_lines_js(content: &str) -> Vec<&str> {
    content.split('\n').collect()
}
