//! Argument-validation errors are model-facing: they must name the argument
//! and the expected shape, and never leak deserializer or Rust internals.

use serde_json::json;
use tf_harness_tools::run_tool;

fn err_of(tool: &str, args: serde_json::Value) -> String {
    run_tool(tool, "/tmp", args).unwrap_err().0
}

#[test]
fn missing_required_argument() {
    assert_eq!(
        err_of("read", json!({})),
        "Invalid arguments: \"path\" is required"
    );
    assert_eq!(
        err_of("bash", json!({})),
        "Invalid arguments: \"command\" is required"
    );
    assert_eq!(
        err_of("write", json!({ "path": "x.txt" })),
        "Invalid arguments: \"content\" is required"
    );
    assert_eq!(
        err_of("grep", json!({})),
        "Invalid arguments: \"pattern\" is required"
    );
}

#[test]
fn wrong_argument_type() {
    assert_eq!(
        err_of("read", json!({ "path": "x.txt", "offset": "5" })),
        "Invalid arguments: \"offset\" must be a number"
    );
    assert_eq!(
        err_of("bash", json!({ "command": 42 })),
        "Invalid arguments: \"command\" must be a string"
    );
    assert_eq!(
        err_of("grep", json!({ "pattern": "x", "ignoreCase": "yes" })),
        "Invalid arguments: \"ignoreCase\" must be a boolean"
    );
}

#[test]
fn non_object_args() {
    assert_eq!(
        err_of("read", json!("just a string")),
        "Invalid arguments: expected a JSON object"
    );
}

#[test]
fn null_optional_treated_as_absent() {
    let dir = tempfile::TempDir::new().unwrap();
    std::fs::write(dir.path().join("a.txt"), "one\ntwo").unwrap();
    let result = run_tool(
        "read",
        dir.path().to_str().unwrap(),
        json!({ "path": "a.txt", "offset": null, "limit": null }),
    )
    .unwrap();
    assert_eq!(result["content"][0]["text"], "one\ntwo");
}

#[test]
fn null_required_is_rejected() {
    assert_eq!(
        err_of("read", json!({ "path": null })),
        "Invalid arguments: \"path\" is required"
    );
}

#[test]
fn no_internals_vocabulary_in_arg_errors() {
    for msg in [
        err_of("read", json!({ "path": 1 })),
        err_of("find", json!({ "pattern": true })),
        err_of("ls", json!({ "limit": "many" })),
    ] {
        for forbidden in ["f64", "u64", "serde", "line 1", "column", "invalid type"] {
            assert!(!msg.contains(forbidden), "internals leaked in: {msg}");
        }
    }
}

#[test]
fn edit_legacy_shape_validates_after_preparation() {
    // Legacy oldText/newText folds into edits before validation, like pi's
    // runtime — so it must not be rejected for a missing "edits".
    let dir = tempfile::TempDir::new().unwrap();
    std::fs::write(dir.path().join("legacy.txt"), "before\n").unwrap();
    let result = run_tool(
        "edit",
        dir.path().to_str().unwrap(),
        json!({ "path": "legacy.txt", "oldText": "before", "newText": "after" }),
    )
    .unwrap();
    assert_eq!(
        result["content"][0]["text"],
        "Successfully replaced 1 block(s) in legacy.txt."
    );

    // While a genuinely missing path is named directly.
    assert_eq!(
        err_of(
            "edit",
            json!({ "edits": [{ "oldText": "a", "newText": "b" }] })
        ),
        "Invalid arguments: \"path\" is required"
    );
}
