//! The file tools refuse what is not a regular file instead of opening it.
//!
//! Every call runs on a thread with a deadline, because the failure these
//! tests guard against is a call that never returns.

#![cfg(unix)]

use std::ffi::CString;
use std::sync::mpsc;
use std::time::Duration;

use serde_json::{json, Value};

fn mkfifo(path: &std::path::Path) {
    let c = CString::new(path.to_str().unwrap()).unwrap();
    assert_eq!(unsafe { libc::mkfifo(c.as_ptr(), 0o644) }, 0, "mkfifo");
}

/// Runs a tool and returns its error text, failing the test if the call
/// blocks.
fn refused(tool: &'static str, cwd: &str, args: Value) -> String {
    let cwd = cwd.to_string();
    let (tx, rx) = mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(tf_harness_tools::run_tool(tool, &cwd, args));
    });
    match rx.recv_timeout(Duration::from_secs(5)) {
        Ok(Err(err)) => err.0,
        Ok(Ok(result)) => panic!("{tool} succeeded on a special file: {result}"),
        Err(_) => panic!("{tool} blocked on a special file"),
    }
}

#[test]
fn every_file_tool_refuses_a_named_pipe_and_a_device() {
    let dir = tempfile::TempDir::new().unwrap();
    let fifo = dir.path().join("pipe");
    mkfifo(&fifo);
    let cwd = dir.path().to_str().unwrap();
    let shown = fifo.to_str().unwrap();

    assert_eq!(
        refused("read", cwd, json!({ "path": "pipe" })),
        format!("Cannot read {shown}: it is a named pipe, not a regular file.")
    );
    assert_eq!(
        refused("write", cwd, json!({ "path": "pipe", "content": "x" })),
        format!("Cannot write {shown}: it is a named pipe, not a regular file.")
    );
    assert_eq!(
        refused(
            "edit",
            cwd,
            json!({ "path": "pipe", "edits": [{ "oldText": "a", "newText": "b" }] })
        ),
        format!("Cannot edit {shown}: it is a named pipe, not a regular file.")
    );
    assert_eq!(
        refused("grep", cwd, json!({ "pattern": "x", "path": "pipe" })),
        format!("Cannot search {shown}: it is a named pipe, not a regular file.")
    );
    assert_eq!(
        refused("read", cwd, json!({ "path": "/dev/zero" })),
        "Cannot read /dev/zero: it is a character device, not a regular file."
    );
}
