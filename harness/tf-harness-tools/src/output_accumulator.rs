//! Port of pi's `core/tools/output-accumulator.ts`: incremental tracking of
//! streaming output with bounded memory — a rolling decoded tail for
//! snapshots plus a temp-file spill of the full raw output once limits are
//! exceeded.

use std::io::Write;

use crate::truncate::{
    truncate_tail, TruncationOptions, TruncationResult, DEFAULT_MAX_BYTES, DEFAULT_MAX_LINES,
};

pub struct OutputSnapshot {
    pub content: String,
    pub truncation: TruncationResult,
    pub full_output_path: Option<String>,
}

fn tmpdir() -> String {
    match std::env::var("TMPDIR") {
        Ok(dir) if !dir.is_empty() => dir.trim_end_matches('/').to_string(),
        _ => "/tmp".to_string(),
    }
}

/// Entropy-free spill-name bytes: pid + monotonic time + a process-global
/// counter. Unique within a process by the counter, across processes by
/// pid+time.
fn fallback_spill_name_bytes() -> [u8; 8] {
    use std::sync::atomic::{AtomicU64, Ordering};
    static COUNTER: AtomicU64 = AtomicU64::new(0);
    let mut ts = libc::timespec {
        tv_sec: 0,
        tv_nsec: 0,
    };
    unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut ts) };
    let mix = (ts.tv_sec as u64)
        .wrapping_mul(1_000_000_007)
        .wrapping_add(ts.tv_nsec as u64)
        .wrapping_add(u64::from(std::process::id()) << 32)
        .wrapping_add(
            COUNTER
                .fetch_add(1, Ordering::Relaxed)
                .wrapping_mul(0x9E37_79B9_7F4A_7C15),
        );
    mix.to_le_bytes()
}

/// Fill spill-name bytes with OS randomness, falling back to the
/// counter-based mix when getrandom fails — an all-zero name would be
/// deterministic and let concurrent runs overwrite each other's spill
/// files.
fn fill_spill_name_bytes(bytes: &mut [u8; 8]) {
    if getrandom::fill(bytes).is_err() {
        *bytes = fallback_spill_name_bytes();
    }
}

fn default_temp_file_path(prefix: &str) -> String {
    let mut bytes = [0u8; 8];
    fill_spill_name_bytes(&mut bytes);
    let id: String = bytes.iter().map(|b| format!("{b:02x}")).collect();
    format!("{}/{prefix}-{id}.log", tmpdir())
}

pub struct OutputAccumulator {
    max_lines: usize,
    max_bytes: usize,
    max_rolling_bytes: usize,
    temp_file_prefix: String,
    decoder: encoding_rs::Decoder,

    raw_chunks: Vec<Vec<u8>>,
    tail_text: String,
    tail_bytes: usize,
    tail_starts_at_line_boundary: bool,
    total_raw_bytes: usize,
    total_decoded_bytes: usize,
    completed_lines: usize,
    total_lines: usize,
    current_line_bytes: usize,
    has_open_line: bool,
    finished: bool,

    temp_file_path: Option<String>,
    temp_file: Option<std::fs::File>,
}

impl OutputAccumulator {
    pub fn new(temp_file_prefix: &str) -> Self {
        let max_bytes = DEFAULT_MAX_BYTES;
        OutputAccumulator {
            max_lines: DEFAULT_MAX_LINES,
            max_bytes,
            max_rolling_bytes: (max_bytes * 2).max(1),
            temp_file_prefix: temp_file_prefix.to_string(),
            decoder: encoding_rs::UTF_8.new_decoder(),
            raw_chunks: Vec::new(),
            tail_text: String::new(),
            tail_bytes: 0,
            tail_starts_at_line_boundary: true,
            total_raw_bytes: 0,
            total_decoded_bytes: 0,
            completed_lines: 0,
            total_lines: 0,
            current_line_bytes: 0,
            has_open_line: false,
            finished: false,
            temp_file_path: None,
            temp_file: None,
        }
    }

    pub fn append(&mut self, data: &[u8]) {
        assert!(
            !self.finished,
            "Cannot append to a finished output accumulator"
        );

        self.total_raw_bytes += data.len();
        let decoded = self.decode_chunk(data, false);
        self.append_decoded_text(&decoded);

        if self.temp_file.is_some() || self.should_use_temp_file() {
            self.ensure_temp_file();
            if let Some(file) = &mut self.temp_file {
                let _ = file.write_all(data);
            }
        } else if !data.is_empty() {
            self.raw_chunks.push(data.to_vec());
        }
    }

    pub fn finish(&mut self) {
        if self.finished {
            return;
        }
        self.finished = true;
        let flushed = self.decode_chunk(&[], true);
        self.append_decoded_text(&flushed);
        if self.should_use_temp_file() {
            self.ensure_temp_file();
        }
    }

    fn decode_chunk(&mut self, data: &[u8], last: bool) -> String {
        let mut out = String::with_capacity(
            self.decoder
                .max_utf8_buffer_length(data.len())
                .unwrap_or(data.len() + 16),
        );
        let (_result, _read, _replaced) = self.decoder.decode_to_string(data, &mut out, last);
        out
    }

    pub fn snapshot(&mut self, persist_if_truncated: bool) -> OutputSnapshot {
        let tail_truncation = truncate_tail(
            &self.get_snapshot_text(),
            TruncationOptions {
                max_lines: Some(self.max_lines),
                max_bytes: Some(self.max_bytes),
            },
        );
        let truncated =
            self.total_lines > self.max_lines || self.total_decoded_bytes > self.max_bytes;
        let truncated_by = if truncated {
            tail_truncation
                .truncated_by
                .or(Some(if self.total_decoded_bytes > self.max_bytes {
                    "bytes"
                } else {
                    "lines"
                }))
        } else {
            None
        };
        let truncation = TruncationResult {
            truncated,
            truncated_by,
            total_lines: self.total_lines,
            total_bytes: self.total_decoded_bytes,
            max_lines: self.max_lines,
            max_bytes: self.max_bytes,
            ..tail_truncation
        };

        if persist_if_truncated && truncation.truncated {
            self.ensure_temp_file();
        }

        OutputSnapshot {
            content: truncation.content.clone(),
            truncation,
            full_output_path: self.temp_file_path.clone(),
        }
    }

    pub fn close_temp_file(&mut self) {
        if let Some(mut file) = self.temp_file.take() {
            let _ = file.flush();
        }
    }

    pub fn get_last_line_bytes(&self) -> usize {
        self.current_line_bytes
    }

    fn append_decoded_text(&mut self, text: &str) {
        if text.is_empty() {
            return;
        }

        let bytes = text.len();
        self.total_decoded_bytes += bytes;
        self.tail_text.push_str(text);
        self.tail_bytes += bytes;
        if self.tail_bytes > self.max_rolling_bytes * 2 {
            self.trim_tail();
        }

        let mut newlines = 0usize;
        let mut last_newline: Option<usize> = None;
        for (i, b) in text.bytes().enumerate() {
            if b == b'\n' {
                newlines += 1;
                last_newline = Some(i);
            }
        }
        if newlines == 0 {
            self.current_line_bytes += bytes;
            self.has_open_line = true;
        } else {
            self.completed_lines += newlines;
            let tail = &text[last_newline.unwrap() + 1..];
            self.current_line_bytes = tail.len();
            self.has_open_line = !tail.is_empty();
        }
        self.total_lines = self.completed_lines + usize::from(self.has_open_line);
    }

    fn trim_tail(&mut self) {
        let buffer = self.tail_text.as_bytes();
        if buffer.len() <= self.max_rolling_bytes {
            self.tail_bytes = buffer.len();
            return;
        }

        let mut start = buffer.len() - self.max_rolling_bytes;
        while start < buffer.len() && (buffer[start] & 0xc0) == 0x80 {
            start += 1;
        }

        self.tail_starts_at_line_boundary = if start == 0 {
            self.tail_starts_at_line_boundary
        } else {
            buffer[start - 1] == 0x0a
        };
        self.tail_text = self.tail_text[start..].to_string();
        self.tail_bytes = self.tail_text.len();
    }

    fn get_snapshot_text(&self) -> String {
        if self.tail_starts_at_line_boundary {
            return self.tail_text.clone();
        }
        match self.tail_text.find('\n') {
            None => self.tail_text.clone(),
            Some(idx) => self.tail_text[idx + 1..].to_string(),
        }
    }

    fn should_use_temp_file(&self) -> bool {
        self.total_raw_bytes > self.max_bytes
            || self.total_decoded_bytes > self.max_bytes
            || self.total_lines > self.max_lines
    }

    fn ensure_temp_file(&mut self) {
        if self.temp_file_path.is_some() {
            return;
        }
        let path = default_temp_file_path(&self.temp_file_prefix);
        if let Ok(mut file) = std::fs::File::create(&path) {
            for chunk in self.raw_chunks.drain(..) {
                let _ = file.write_all(&chunk);
            }
            self.temp_file = Some(file);
            self.temp_file_path = Some(path);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::fallback_spill_name_bytes;

    #[test]
    fn fallback_names_are_unique_and_nonzero() {
        let a = fallback_spill_name_bytes();
        let b = fallback_spill_name_bytes();
        assert_ne!(a, [0u8; 8]);
        assert_ne!(a, b);
    }
}
