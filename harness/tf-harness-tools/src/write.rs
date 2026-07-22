//! Port of pi's `core/tools/write.ts` execute path.

use serde::Deserialize;

use crate::errors::node_fs_error;
use crate::jsstring::utf16_len;
use crate::mutation_queue::with_file_mutation_queue;
use crate::paths::{node_dirname, resolve_to_cwd};
use crate::result::{ToolError, ToolResult, ToolResultOrError};

#[derive(Debug, Clone, Deserialize)]
pub struct WriteArgs {
    pub path: String,
    pub content: String,
}

pub fn execute(cwd: &str, args: &WriteArgs) -> ToolResultOrError<()> {
    let absolute_path = resolve_to_cwd(&args.path, cwd);
    let dir = node_dirname(&absolute_path);

    with_file_mutation_queue(&absolute_path, || {
        std::fs::create_dir_all(&dir)
            .map_err(|e| ToolError::new(node_fs_error(&e, "mkdir", &dir).message))?;
        std::fs::write(&absolute_path, args.content.as_bytes())
            .map_err(|e| ToolError::new(node_fs_error(&e, "open", &absolute_path).message))?;

        // "bytes" is pi's label for JS string length — UTF-16 code units.
        Ok(ToolResult::text_only(format!(
            "Successfully wrote {} bytes to {}",
            utf16_len(&args.content),
            args.path
        )))
    })
}
