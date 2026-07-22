//! Port of pi's `core/tools/edit.ts` execute path, including
//! `prepareArguments` (legacy oldText/newText folding and JSON-string edits).

use serde_json::Value;

use crate::edit_diff::{
    apply_edits_to_normalized_content, detect_line_ending, generate_diff_string,
    generate_unified_patch, normalize_to_lf, restore_line_endings, strip_bom, Edit,
};
use crate::mutation_queue::with_file_mutation_queue;
use crate::paths::resolve_to_cwd;
use crate::result::{ContentBlock, EditToolDetails, ToolError, ToolResult, ToolResultOrError};

/// pi's `prepareArguments`: parse a stringified `edits` array, then fold
/// legacy top-level oldText/newText into `edits`.
pub fn prepare_arguments(input: Value) -> Value {
    let Value::Object(mut args) = input else {
        return input;
    };

    if let Some(Value::String(s)) = args.get("edits") {
        if let Ok(parsed) = serde_json::from_str::<Value>(s) {
            if parsed.is_array() {
                args.insert("edits".to_string(), parsed);
            }
        }
    }

    let old_text = args
        .get("oldText")
        .and_then(Value::as_str)
        .map(str::to_string);
    let new_text = args
        .get("newText")
        .and_then(Value::as_str)
        .map(str::to_string);
    if let (Some(old_text), Some(new_text)) = (old_text, new_text) {
        let mut edits = match args.get("edits") {
            Some(Value::Array(existing)) => existing.clone(),
            _ => Vec::new(),
        };
        edits.push(serde_json::json!({ "oldText": old_text, "newText": new_text }));
        args.remove("oldText");
        args.remove("newText");
        args.insert("edits".to_string(), Value::Array(edits));
    }

    Value::Object(args)
}

pub struct EditArgs {
    pub path: String,
    pub edits: Vec<Edit>,
}

/// pi's `validateEditInput` + typebox: edits must be a non-empty array of
/// {oldText, newText} objects.
pub fn parse_args(input: &Value) -> Result<EditArgs, ToolError> {
    let invalid = || {
        ToolError::new("Edit tool input is invalid. edits must contain at least one replacement.")
    };
    let obj = input.as_object().ok_or_else(invalid)?;
    let path = obj
        .get("path")
        .and_then(Value::as_str)
        .ok_or_else(invalid)?
        .to_string();
    let edits_value = obj
        .get("edits")
        .and_then(Value::as_array)
        .ok_or_else(invalid)?;
    if edits_value.is_empty() {
        return Err(invalid());
    }
    let mut edits = Vec::with_capacity(edits_value.len());
    for edit in edits_value {
        let old_text = edit
            .get("oldText")
            .and_then(Value::as_str)
            .ok_or_else(invalid)?;
        let new_text = edit
            .get("newText")
            .and_then(Value::as_str)
            .ok_or_else(invalid)?;
        edits.push(Edit {
            old_text: old_text.to_string(),
            new_text: new_text.to_string(),
        });
    }
    Ok(EditArgs { path, edits })
}

fn access_read_write(path: &str) -> Result<(), std::io::Error> {
    let c_path = std::ffi::CString::new(path.as_bytes())
        .map_err(|_| std::io::Error::from_raw_os_error(libc::ENOENT))?;
    let rc = unsafe { libc::access(c_path.as_ptr(), libc::R_OK | libc::W_OK) };
    if rc == 0 {
        Ok(())
    } else {
        Err(std::io::Error::last_os_error())
    }
}

pub fn execute(cwd: &str, args: &EditArgs) -> ToolResultOrError<EditToolDetails> {
    let path = &args.path;
    let absolute_path = resolve_to_cwd(path, cwd);

    with_file_mutation_queue(&absolute_path, || {
        if let Err(error) = access_read_write(&absolute_path) {
            // pi surfaces just the errno code here.
            let code = crate::errors::node_fs_error(&error, "access", &absolute_path).code;
            return Err(ToolError::new(format!(
                "Could not edit file: {path}. Error code: {code}."
            )));
        }

        let buffer = std::fs::read(&absolute_path).map_err(|e| {
            ToolError::new(crate::errors::node_fs_error(&e, "open", &absolute_path).message)
        })?;
        let raw_content = String::from_utf8_lossy(&buffer);

        // Strip the BOM before matching; the model never includes it.
        let (bom, content) = strip_bom(&raw_content);
        let original_ending = detect_line_ending(content);
        let normalized_content = normalize_to_lf(content);
        let applied = apply_edits_to_normalized_content(&normalized_content, &args.edits, path)?;

        let final_content = format!(
            "{bom}{}",
            restore_line_endings(&applied.new_content, original_ending)
        );
        std::fs::write(&absolute_path, final_content.as_bytes()).map_err(|e| {
            ToolError::new(crate::errors::node_fs_error(&e, "open", &absolute_path).message)
        })?;

        let diff_result = generate_diff_string(&applied.base_content, &applied.new_content);
        let patch = generate_unified_patch(path, &applied.base_content, &applied.new_content);
        Ok(ToolResult {
            content: vec![ContentBlock::text(format!(
                "Successfully replaced {} block(s) in {path}.",
                args.edits.len()
            ))],
            details: Some(EditToolDetails {
                diff: diff_result.diff,
                patch,
                first_changed_line: diff_result.first_changed_line,
            }),
        })
    })
}
