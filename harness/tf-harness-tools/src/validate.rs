//! Argument validation against the exported tool schemas.
//!
//! Errors here are model-facing: they must say exactly which argument is
//! wrong and what shape it should be, in schema vocabulary ("a number",
//! "a string") — never deserializer internals ("expected f64 at line 1
//! column 22"). `null` for an optional argument is treated as absent;
//! models emit it often and rejecting it helps nobody.

use serde_json::Value;

use crate::definitions::tool_definition;
use crate::result::ToolError;

fn type_matches(schema_type: &str, value: &Value) -> bool {
    match schema_type {
        "string" => value.is_string(),
        "number" => value.is_number(),
        "boolean" => value.is_boolean(),
        "array" => value.is_array(),
        "object" => value.is_object(),
        _ => true,
    }
}

fn type_label(schema_type: &str) -> &'static str {
    match schema_type {
        "string" => "a string",
        "number" => "a number",
        "boolean" => "a boolean",
        "array" => "an array",
        "object" => "an object",
        _ => "a value",
    }
}

/// Check `args` against the tool's parameter schema (shallow: required keys
/// and top-level types; unknown keys pass through, like TypeBox without
/// additionalProperties).
pub fn validate_args(tool: &str, args: &Value) -> Result<(), ToolError> {
    let Some(def) = tool_definition(tool) else {
        return Ok(());
    };
    let params = &def["parameters"];

    let Some(args_obj) = args.as_object() else {
        return Err(ToolError::new("Invalid arguments: expected a JSON object"));
    };

    if let Some(required) = params["required"].as_array() {
        for key in required.iter().filter_map(Value::as_str) {
            match args_obj.get(key) {
                None | Some(Value::Null) => {
                    return Err(ToolError::new(format!(
                        "Invalid arguments: \"{key}\" is required"
                    )));
                }
                _ => {}
            }
        }
    }

    if let Some(props) = params["properties"].as_object() {
        for (key, value) in args_obj {
            if value.is_null() {
                continue;
            }
            let Some(schema) = props.get(key) else {
                continue;
            };
            let Some(schema_type) = schema["type"].as_str() else {
                continue;
            };
            if !type_matches(schema_type, value) {
                return Err(ToolError::new(format!(
                    "Invalid arguments: \"{key}\" must be {}",
                    type_label(schema_type)
                )));
            }
        }
    }

    Ok(())
}
