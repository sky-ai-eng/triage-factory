//! Port of pi's `core/tools/read.ts` execute path.

use std::io::Read;

use serde::Deserialize;

use crate::errors::node_fs_error;
use crate::image::{detect_supported_image_mime_type_from_file, process_image, ProcessImageResult};
use crate::jsnum::{js_num, to_index};
use crate::paths::resolve_read_path;
use crate::result::{ContentBlock, ReadToolDetails, ToolError, ToolResult, ToolResultOrError};
use crate::special::refuse_special_file;
use crate::truncate::{
    format_size, truncate_head, truncate_head_source, HeadSource, TruncationOptions,
    DEFAULT_MAX_BYTES, DEFAULT_MAX_LINES,
};

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
    refuse_special_file(&absolute_path, "read")?;

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

    // 1-indexed offset; JS `offset ? max(0, offset - 1) : 0` (0 is falsy).
    let offset_raw = args.offset.unwrap_or(0.0);
    let start_line_f = if offset_raw != 0.0 && !offset_raw.is_nan() {
        (offset_raw - 1.0).max(0.0)
    } else {
        0.0
    };
    let start_line = to_index(start_line_f);
    // The line the selection ends before, bounded by the file's own end once
    // that is known. JS `min(start + limit, lines.length)` takes the length
    // for a NaN sum.
    let end_bound = match args.limit {
        Some(limit) => {
            let end = start_line as f64 + limit;
            if end.is_nan() {
                usize::MAX
            } else {
                to_index(end)
            }
        }
        None => usize::MAX,
    };

    let scan = scan_text_file(
        &absolute_path,
        start_line,
        end_bound,
        READ_CHUNK_BYTES,
        TruncationOptions::default(),
    )
    .map_err(|e| ToolError::new(node_fs_error(&e, "open", &absolute_path).message))?;
    let total_file_lines = scan.total_lines;

    if start_line_f >= total_file_lines as f64 {
        return Err(ToolError::new(format!(
            "Offset {} is beyond end of file ({} lines total)",
            js_num(offset_raw),
            total_file_lines
        )));
    }
    let start_line_display = start_line + 1;
    let user_limited_lines = args
        .limit
        .map(|_| end_bound.min(total_file_lines).max(start_line) - start_line);

    let truncation = scan.truncate_head(TruncationOptions::default());
    let mut details: Option<ReadToolDetails> = None;
    let output_text: String;
    if truncation.first_line_exceeds_limit {
        // First line alone exceeds the byte limit; point the model at a bash
        // fallback.
        let first_line_size = format_size(scan.first_selected_bytes);
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
    } else if let Some(limited) = user_limited_lines.filter(|l| start_line + l < total_file_lines) {
        let remaining = total_file_lines - (start_line + limited);
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

/// How much of a file a text read takes in at a time.
const READ_CHUNK_BYTES: usize = 64 * 1024;

/// One pass over a text file: its line count, and of the selected lines (those
/// from `start` up to `end`) their totals and as many of their leading lines,
/// decoded, as the output can hold.
///
/// pi reads the whole file into memory, decodes it, and splits it. Read that
/// way, a file larger than the jail's memory ceiling would end the tool host,
/// and the jail with it. This reads the file in chunks and keeps only what
/// truncation can show, which the output limits bound whatever the file's
/// size. The
/// result is the same, because a newline is never part of an invalid UTF-8
/// sequence: decoding line by line gives the lines decoding the whole file
/// would.
struct TextScan {
    /// Lines in the whole file, as `text.split('\n')` counts them.
    total_lines: usize,
    /// Selected lines.
    selected: usize,
    /// Whether the last selected line is empty.
    last_selected_empty: bool,
    /// Decoded bytes of the selected lines joined by newlines.
    selected_bytes: usize,
    /// Decoded bytes of the first selected line.
    first_selected_bytes: usize,
    /// The leading selected lines, decoded: every one while they fit the
    /// limits, and then none.
    kept: Vec<String>,
}

impl TextScan {
    /// `truncate_head` of the selected lines joined by newlines.
    fn truncate_head(&self, options: TruncationOptions) -> crate::truncate::TruncationResult {
        if self.selected == 0 {
            return truncate_head("", options);
        }
        let lines: Vec<&str> = self.kept.iter().map(String::as_str).collect();
        truncate_head_source(
            &HeadSource {
                lines: &lines,
                split_count: self.selected,
                ends_with_newline: self.selected > 1 && self.last_selected_empty,
                total_bytes: self.selected_bytes,
                first_line_bytes: self.first_selected_bytes,
            },
            options,
        )
    }
}

fn scan_text_file(
    path: &str,
    start: usize,
    end: usize,
    chunk_bytes: usize,
    options: TruncationOptions,
) -> std::io::Result<TextScan> {
    let mut file = std::fs::File::open(path)?;
    let mut scanner = LineScanner::new(start, end, options);
    let mut buf = vec![0u8; chunk_bytes];
    loop {
        let n = match file.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        scanner.feed(&buf[..n]);
    }
    Ok(scanner.finish())
}

/// The state of a `TextScan` between chunks.
struct LineScanner {
    start: usize,
    end: usize,
    max_lines: usize,
    max_bytes: usize,
    /// Index of the line being read.
    line: usize,
    /// Decoded length of the line being read, when it is selected.
    len: LossyLen,
    /// Raw bytes of the line being read, while it may yet be kept.
    raw: Vec<u8>,
    /// Whether the line being read has any bytes.
    nonempty: bool,
    /// Whether lines are still being kept; the first that is not ends it.
    keeping: bool,
    /// Decoded bytes of the kept lines joined by newlines.
    kept_bytes: usize,
    scan: TextScan,
}

impl LineScanner {
    fn new(start: usize, end: usize, options: TruncationOptions) -> Self {
        LineScanner {
            start,
            end,
            max_lines: options.max_lines.unwrap_or(DEFAULT_MAX_LINES),
            max_bytes: options.max_bytes.unwrap_or(DEFAULT_MAX_BYTES),
            line: 0,
            len: LossyLen::default(),
            raw: Vec::new(),
            nonempty: false,
            keeping: true,
            kept_bytes: 0,
            scan: TextScan {
                total_lines: 0,
                selected: 0,
                last_selected_empty: false,
                selected_bytes: 0,
                first_selected_bytes: 0,
                kept: Vec::new(),
            },
        }
    }

    fn selected(&self) -> bool {
        self.line >= self.start && self.line < self.end
    }

    fn feed(&mut self, mut bytes: &[u8]) {
        while !bytes.is_empty() {
            if self.line >= self.end {
                // Past the selection only the lines are counted.
                self.line += bytes.iter().filter(|&&b| b == b'\n').count();
                return;
            }
            match bytes.iter().position(|&b| b == b'\n') {
                Some(i) => {
                    self.take(&bytes[..i]);
                    self.end_line();
                    bytes = &bytes[i + 1..];
                }
                None => {
                    self.take(bytes);
                    return;
                }
            }
        }
    }

    /// Takes part of the line being read.
    fn take(&mut self, segment: &[u8]) {
        if segment.is_empty() || !self.selected() {
            return;
        }
        self.nonempty = true;
        self.len.feed(segment);
        if self.may_keep() {
            // A line decodes to at least as many bytes as it has, so one that
            // has more than the room left cannot be kept.
            if self.raw.len() + segment.len() > self.room() {
                self.keeping = false;
                self.raw = Vec::new();
            } else {
                self.raw.extend_from_slice(segment);
            }
        }
    }

    /// Whether the line being read may still be kept. Truncation reads at
    /// most one line past its line limit, to see whether a trailing newline
    /// ends the content there.
    fn may_keep(&self) -> bool {
        self.keeping && self.scan.kept.len() <= self.max_lines
    }

    /// Decoded bytes the line being read may take and still be kept.
    fn room(&self) -> usize {
        let separator = usize::from(self.scan.selected > 0);
        self.max_bytes.saturating_sub(self.kept_bytes + separator)
    }

    /// Ends the line being read: at a newline, and at the end of the file.
    fn end_line(&mut self) {
        if self.selected() {
            let n = self.len.finish();
            let separator = usize::from(self.scan.selected > 0);
            if self.scan.selected == 0 {
                self.scan.first_selected_bytes = n;
            }
            self.scan.selected_bytes += n + separator;
            self.scan.last_selected_empty = !self.nonempty;
            if self.may_keep() && n <= self.room() {
                self.scan
                    .kept
                    .push(String::from_utf8_lossy(&self.raw).into_owned());
                self.kept_bytes += n + separator;
            } else {
                self.keeping = false;
            }
            self.scan.selected += 1;
            self.raw.clear();
            self.nonempty = false;
        }
        self.line += 1;
    }

    fn finish(mut self) -> TextScan {
        // The text after the last newline is a line too, empty or not, as
        // `split('\n')` counts it.
        self.end_line();
        self.scan.total_lines = self.line;
        self.scan
    }
}

/// The length of a byte stream decoded as `String::from_utf8_lossy` decodes
/// it, fed in pieces: a sequence split across two pieces is carried until it
/// can be judged, and one still incomplete when the stream ends is one
/// replacement character, as it is at the end of a whole buffer.
#[derive(Default)]
struct LossyLen {
    len: usize,
    carry: Vec<u8>,
}

impl LossyLen {
    fn feed(&mut self, bytes: &[u8]) {
        if self.carry.is_empty() {
            self.count(bytes);
        } else {
            let mut joined = std::mem::take(&mut self.carry);
            joined.extend_from_slice(bytes);
            self.count(&joined);
        }
    }

    fn count(&mut self, mut rest: &[u8]) {
        loop {
            match std::str::from_utf8(rest) {
                Ok(valid) => {
                    self.len += valid.len();
                    return;
                }
                Err(e) => {
                    self.len += e.valid_up_to();
                    match e.error_len() {
                        Some(invalid) => {
                            self.len += char::REPLACEMENT_CHARACTER.len_utf8();
                            rest = &rest[e.valid_up_to() + invalid..];
                        }
                        None => {
                            self.carry = rest[e.valid_up_to()..].to_vec();
                            return;
                        }
                    }
                }
            }
        }
    }

    /// The length so far, ending the stream; the counter starts over.
    fn finish(&mut self) -> usize {
        if !self.carry.is_empty() {
            self.len += char::REPLACEMENT_CHARACTER.len_utf8();
            self.carry.clear();
        }
        std::mem::take(&mut self.len)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The text read as it was before it streamed: the whole file decoded and
    /// split. The streaming read must answer exactly as this does.
    fn whole_file_read(buffer: &[u8], args: &ReadArgs) -> ToolResultOrError<ReadToolDetails> {
        let text_content = String::from_utf8_lossy(buffer);
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
        } else if let Some(limited) =
            user_limited_lines.filter(|l| start_line + l < all_lines.len())
        {
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

    fn answer(r: &ToolResultOrError<ReadToolDetails>) -> String {
        match r {
            Ok(result) => serde_json::to_string(result).unwrap(),
            Err(e) => format!("error: {}", e.0),
        }
    }

    /// xorshift64*, so the cases are reproducible without a dependency.
    struct Rng(u64);
    impl Rng {
        fn next(&mut self) -> u64 {
            self.0 ^= self.0 >> 12;
            self.0 ^= self.0 << 25;
            self.0 ^= self.0 >> 27;
            self.0.wrapping_mul(0x2545_f491_4f6c_dd1d)
        }
        fn below(&mut self, n: u64) -> u64 {
            self.next() % n
        }
    }

    /// Content that crosses every boundary the scan handles: newlines, runs
    /// of empty lines, multi-byte characters, invalid and truncated
    /// sequences, lines past the byte limit, and more lines than the line
    /// limit.
    fn content(rng: &mut Rng) -> Vec<u8> {
        let pieces: [&[u8]; 12] = [
            b"a",
            b"hello world ",
            b"\n",
            b"\n\n\n",
            "\u{e9}".as_bytes(),
            "\u{20ac}".as_bytes(),
            "\u{1f600}".as_bytes(),
            b"\xff",
            b"\x80",
            b"\xe2\x82",
            b"\xf0\x9f\x98",
            b"\r\n",
        ];
        let mut out = Vec::new();
        let shape = rng.below(4);
        let target = match shape {
            0 => rng.below(64) as usize,
            1 => rng.below(4096) as usize,
            2 => 40 * 1024 + rng.below(30 * 1024) as usize,
            _ => 120 * 1024 + rng.below(80 * 1024) as usize,
        };
        while out.len() < target {
            if shape >= 2 && rng.below(50) == 0 {
                out.extend(std::iter::repeat_n(b'x', rng.below(60 * 1024) as usize));
            }
            out.extend_from_slice(pieces[rng.below(pieces.len() as u64) as usize]);
        }
        if shape == 3 && rng.below(2) == 0 {
            let mut short: Vec<u8> = std::iter::repeat_n(b"ab\n".as_slice(), 2500)
                .flatten()
                .copied()
                .collect();
            short.append(&mut out);
            out = short;
        }
        out
    }

    fn number(rng: &mut Rng, lines: usize) -> Option<f64> {
        Some(match rng.below(10) {
            0 => return None,
            1 => 0.0,
            2 => -3.0,
            3 => f64::NAN,
            4 => f64::INFINITY,
            5 => 1.5,
            6 => (lines + 1 + rng.below(3) as usize) as f64,
            _ => 1.0 + rng.below(lines as u64 + 1) as f64,
        })
    }

    /// The streaming read answers every case the whole-file read does, byte
    /// for byte, details included.
    #[test]
    fn streaming_read_answers_as_the_whole_file_read() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("f.txt");
        let mut rng = Rng(0x9e37_79b9_7f4a_7c15);
        let mut shapes = [0usize; 6];
        for case in 0..600 {
            let bytes = content(&mut rng);
            std::fs::write(&path, &bytes).unwrap();
            let lines = bytes.iter().filter(|&&b| b == b'\n').count() + 1;
            let args = ReadArgs {
                path: "f.txt".into(),
                offset: number(&mut rng, lines),
                limit: number(&mut rng, lines),
            };
            let want = answer(&whole_file_read(&bytes, &args));
            let got = answer(&execute(
                dir.path().to_str().unwrap(),
                &args,
                &ReadOptions::default(),
            ));
            assert_eq!(
                got,
                want,
                "case {case}: offset {:?} limit {:?}, {} bytes",
                args.offset,
                args.limit,
                bytes.len()
            );
            let shape = [
                "beyond end of file",
                "exceeds 50.0KB limit",
                "limit). Use offset=",
                "Showing lines",
                "more lines in file",
                "",
            ]
            .iter()
            .position(|marker| want.contains(marker))
            .unwrap();
            shapes[shape] += 1;
        }
        // Every way a read can answer came up, so the comparison covered each.
        assert!(
            shapes.iter().all(|&n| n > 0),
            "answers by shape: {shapes:?}"
        );
    }

    /// Where the chunks fall changes nothing the scan reports, down to one
    /// byte at a time, which splits every multi-byte sequence there is.
    #[test]
    fn chunk_boundaries_do_not_change_the_scan() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("f.txt");
        let path = path.to_str().unwrap();
        let mut rng = Rng(0x2545_f491_4f6c_dd1d);
        for case in 0..60 {
            let bytes = content(&mut rng);
            std::fs::write(path, &bytes).unwrap();
            let start = rng.below(40) as usize;
            let end = if rng.below(2) == 0 {
                usize::MAX
            } else {
                start + rng.below(3000) as usize
            };
            let scan = |chunk| {
                scan_text_file(path, start, end, chunk, TruncationOptions::default()).unwrap()
            };
            let want = scan(READ_CHUNK_BYTES);
            for chunk in [1, 2, 3, 5, 4096] {
                let got = scan(chunk);
                let same = got.total_lines == want.total_lines
                    && got.selected == want.selected
                    && got.last_selected_empty == want.last_selected_empty
                    && got.selected_bytes == want.selected_bytes
                    && got.first_selected_bytes == want.first_selected_bytes
                    && got.kept == want.kept;
                assert!(
                    same,
                    "case {case}: chunks of {chunk} bytes scanned differently"
                );
            }
        }
    }

    /// What a read holds is bounded by the output limits, not the file: a
    /// file far past them keeps at most one line past the line limit and no
    /// more than the byte limit.
    #[test]
    fn a_huge_file_keeps_only_what_the_output_can_show() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("big.txt");
        let line = b"0123456789abcdefghijklmnopqrstuvwxyz0123\n";
        let mut bytes = Vec::with_capacity(line.len() * 200_000);
        for _ in 0..200_000 {
            bytes.extend_from_slice(line);
        }
        std::fs::write(&path, &bytes).unwrap();
        let scan = scan_text_file(
            path.to_str().unwrap(),
            0,
            usize::MAX,
            READ_CHUNK_BYTES,
            TruncationOptions::default(),
        )
        .unwrap();
        assert_eq!(scan.total_lines, 200_001);
        assert_eq!(scan.selected, 200_001);
        let kept: usize = scan.kept.iter().map(|l| l.len() + 1).sum();
        assert!(
            scan.kept.len() <= DEFAULT_MAX_LINES + 1 && kept <= DEFAULT_MAX_BYTES + 1,
            "kept {} lines, {kept} bytes",
            scan.kept.len()
        );
    }
}
