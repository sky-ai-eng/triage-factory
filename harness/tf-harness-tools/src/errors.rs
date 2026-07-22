//! Node/libuv-style error strings.
//!
//! pi's tools surface raw Node `fs` errors to the model (for example
//! `ENOENT: no such file or directory, access '/path'`), and the edit tool
//! wraps just the code (`Error code: ENOENT`). Errors are formatted here to
//! match libuv's `${code}: ${message}, ${syscall} '${path}'` shape.

use std::io;

pub struct NodeError {
    pub code: &'static str,
    pub message: String,
}

fn uv_strings(errno: i32) -> (&'static str, &'static str) {
    match errno {
        libc::ENOENT => ("ENOENT", "no such file or directory"),
        libc::EACCES => ("EACCES", "permission denied"),
        libc::EPERM => ("EPERM", "operation not permitted"),
        libc::ENOTDIR => ("ENOTDIR", "not a directory"),
        libc::EISDIR => ("EISDIR", "illegal operation on a directory"),
        libc::EEXIST => ("EEXIST", "file already exists"),
        libc::ENOTEMPTY => ("ENOTEMPTY", "directory not empty"),
        libc::EMFILE => ("EMFILE", "too many open files"),
        libc::ENFILE => ("ENFILE", "file table overflow"),
        libc::ENAMETOOLONG => ("ENAMETOOLONG", "name too long"),
        libc::ELOOP => ("ELOOP", "too many symbolic links encountered"),
        libc::ENOSPC => ("ENOSPC", "no space left on device"),
        libc::EROFS => ("EROFS", "read-only file system"),
        libc::EBADF => ("EBADF", "bad file descriptor"),
        libc::EINVAL => ("EINVAL", "invalid argument"),
        libc::EIO => ("EIO", "i/o error"),
        libc::EFBIG => ("EFBIG", "file too large"),
        libc::ENXIO => ("ENXIO", "no such device or address"),
        libc::ENODEV => ("ENODEV", "no such device"),
        libc::EBUSY => ("EBUSY", "resource busy or locked"),
        libc::ETXTBSY => ("ETXTBSY", "text file is busy"),
        libc::EAGAIN => ("EAGAIN", "resource temporarily unavailable"),
        _ => ("UNKNOWN", "unknown error"),
    }
}

/// Format an io::Error the way Node fs would for the given syscall + path,
/// e.g. `ENOENT: no such file or directory, open '/tmp/x'`.
pub fn node_fs_error(err: &io::Error, syscall: &str, path: &str) -> NodeError {
    let errno = err.raw_os_error().unwrap_or(0);
    let (code, message) = uv_strings(errno);
    NodeError {
        code,
        message: format!("{code}: {message}, {syscall} '{path}'"),
    }
}
