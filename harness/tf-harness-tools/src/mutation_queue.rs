//! Port of pi's `file-mutation-queue.ts`: serialize mutations targeting the
//! same file (keyed by realpath so symlinked aliases share a queue) while
//! different files proceed in parallel.

use std::collections::HashMap;
use std::io;
use std::sync::{Arc, Mutex, OnceLock};

use crate::errors::node_fs_error;
use crate::result::ToolError;

static QUEUES: OnceLock<Mutex<HashMap<String, Arc<Mutex<()>>>>> = OnceLock::new();

fn queues() -> &'static Mutex<HashMap<String, Arc<Mutex<()>>>> {
    QUEUES.get_or_init(|| Mutex::new(HashMap::new()))
}

fn is_missing_path_error(err: &io::Error) -> bool {
    matches!(err.raw_os_error(), Some(code) if code == libc::ENOENT || code == libc::ENOTDIR)
}

fn mutation_queue_key(file_path: &str) -> Result<String, ToolError> {
    match std::fs::canonicalize(file_path) {
        Ok(real) => Ok(real.to_string_lossy().into_owned()),
        Err(err) if is_missing_path_error(&err) => Ok(file_path.to_string()),
        Err(err) => Err(ToolError::new(
            node_fs_error(&err, "lstat", file_path).message,
        )),
    }
}

pub fn with_file_mutation_queue<T>(
    file_path: &str,
    f: impl FnOnce() -> Result<T, ToolError>,
) -> Result<T, ToolError> {
    let key = mutation_queue_key(file_path)?;
    let entry = {
        let mut map = queues().lock().unwrap();
        map.entry(key.clone())
            .or_insert_with(|| Arc::new(Mutex::new(())))
            .clone()
    };
    let result = {
        let _guard = entry.lock().unwrap();
        f()
    };
    {
        let mut map = queues().lock().unwrap();
        // The map holds one Arc; ours is the second. No third means no waiter.
        if Arc::strong_count(&entry) <= 2 {
            map.remove(&key);
        }
    }
    result
}
