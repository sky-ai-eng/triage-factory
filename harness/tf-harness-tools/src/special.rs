//! The file tools' refusal of anything that is not a regular file.
//!
//! A divergence from pi, whose tools open whatever the path names. Opening a
//! named pipe blocks until a process opens its other end, which in a sandbox
//! may never happen, and reading a character device such as `/dev/zero` never
//! reaches an end. Either holds the one tool call the session can have in
//! flight, and with it the whole engagement, until the loop gives up on it.
//! The check is a `stat` before the open, so a pipe created between the two
//! can still block; the loop's deadline for a tool call stays underneath.

use std::os::unix::fs::FileTypeExt;

use crate::result::ToolError;

/// Refuses `path` when it exists and is a named pipe, a socket, or a device.
/// Anything else is left to the tool, so a missing file or a directory fails
/// with the error pi's tool gives it. `verb` names what the tool was going to
/// do: "read", "write", "edit", "search".
pub fn refuse_special_file(path: &str, verb: &str) -> Result<(), ToolError> {
    let Ok(meta) = std::fs::metadata(path) else {
        return Ok(());
    };
    let file_type = meta.file_type();
    let kind = if file_type.is_fifo() {
        "a named pipe"
    } else if file_type.is_socket() {
        "a socket"
    } else if file_type.is_char_device() {
        "a character device"
    } else if file_type.is_block_device() {
        "a block device"
    } else {
        return Ok(());
    };
    Err(ToolError::new(format!(
        "Cannot {verb} {path}: it is {kind}, not a regular file."
    )))
}
