//! Port of pi's `test/tools.test.ts` (read/write/bash/grep/find/ls sections).
//! Expected strings are copied verbatim from pi's tests.

use base64::Engine;
use serde::Serialize;
use tf_harness_tools::result::{ContentBlock, ToolError, ToolResult};

fn text_output<D: Serialize>(result: &ToolResult<D>) -> String {
    result.text_output()
}

fn tmp_dir() -> tempfile::TempDir {
    tempfile::TempDir::new().unwrap()
}

fn write_file(dir: &std::path::Path, name: &str, content: impl AsRef<[u8]>) -> String {
    let path = dir.join(name);
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).unwrap();
    }
    std::fs::write(&path, content.as_ref()).unwrap();
    path.to_string_lossy().into_owned()
}

fn read_tool(
    cwd: &str,
    args: serde_json::Value,
) -> Result<ToolResult<tf_harness_tools::result::ReadToolDetails>, ToolError> {
    let parsed: tf_harness_tools::read::ReadArgs = serde_json::from_value(args).unwrap();
    tf_harness_tools::read::execute(
        cwd,
        &parsed,
        &tf_harness_tools::read::ReadOptions::default(),
    )
}

fn bash_tool(
    cwd: &str,
    args: serde_json::Value,
) -> Result<ToolResult<tf_harness_tools::result::BashToolDetails>, ToolError> {
    let parsed: tf_harness_tools::bash::BashArgs = serde_json::from_value(args).unwrap();
    tf_harness_tools::bash::execute(
        cwd,
        &parsed,
        &tf_harness_tools::bash::BashOptions::default(),
    )
}

fn grep_tool(
    cwd: &str,
    args: serde_json::Value,
) -> Result<ToolResult<tf_harness_tools::result::GrepToolDetails>, ToolError> {
    let parsed: tf_harness_tools::grep::GrepArgs = serde_json::from_value(args).unwrap();
    tf_harness_tools::grep::execute(cwd, &parsed)
}

fn find_tool(
    cwd: &str,
    args: serde_json::Value,
) -> Result<ToolResult<tf_harness_tools::result::FindToolDetails>, ToolError> {
    let parsed: tf_harness_tools::find::FindArgs = serde_json::from_value(args).unwrap();
    tf_harness_tools::find::execute(cwd, &parsed)
}

fn ls_tool(
    cwd: &str,
    args: serde_json::Value,
) -> Result<ToolResult<tf_harness_tools::result::LsToolDetails>, ToolError> {
    let parsed: tf_harness_tools::ls::LsArgs = serde_json::from_value(args).unwrap();
    tf_harness_tools::ls::execute(cwd, &parsed)
}

fn image_block<D: Serialize>(result: &ToolResult<D>) -> Option<(String, String)> {
    result.content.iter().find_map(|c| match c {
        ContentBlock::Image { data, mime_type } => Some((data.clone(), mime_type.clone())),
        _ => None,
    })
}

fn create_tiny_bmp_1x1_red_24bpp() -> Vec<u8> {
    let mut buffer = vec![0u8; 58];
    buffer[0] = b'B';
    buffer[1] = b'M';
    buffer[2..6].copy_from_slice(&(58u32).to_le_bytes());
    buffer[10..14].copy_from_slice(&(54u32).to_le_bytes());
    buffer[14..18].copy_from_slice(&(40u32).to_le_bytes());
    buffer[18..22].copy_from_slice(&(1i32).to_le_bytes());
    buffer[22..26].copy_from_slice(&(1i32).to_le_bytes());
    buffer[26..28].copy_from_slice(&(1u16).to_le_bytes());
    buffer[28..30].copy_from_slice(&(24u16).to_le_bytes());
    buffer[30..34].copy_from_slice(&(0u32).to_le_bytes());
    buffer[34..38].copy_from_slice(&(4u32).to_le_bytes());
    buffer[56] = 0xff;
    buffer
}

// ---------------------------------------------------------------- read tool

#[test]
fn read_file_contents_within_limits() {
    let dir = tmp_dir();
    let content = "Hello, world!\nLine 2\nLine 3";
    let test_file = write_file(dir.path(), "test.txt", content);

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file }),
    )
    .unwrap();

    assert_eq!(text_output(&result), content);
    assert!(!text_output(&result).contains("Use offset="));
    assert!(result.details.is_none());
}

#[test]
fn read_nonexistent_file() {
    let dir = tmp_dir();
    let test_file = dir.path().join("nonexistent.txt");

    let err = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file.to_str().unwrap() }),
    )
    .unwrap_err();
    assert!(err.0.contains("ENOENT"), "unexpected error: {}", err.0);
}

#[test]
fn read_truncates_line_limit() {
    let dir = tmp_dir();
    let lines: Vec<String> = (1..=2500).map(|i| format!("Line {i}")).collect();
    let test_file = write_file(dir.path(), "large.txt", lines.join("\n"));

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file }),
    )
    .unwrap();
    let output = text_output(&result);

    assert!(output.contains("Line 1"));
    assert!(output.contains("Line 2000"));
    assert!(!output.contains("Line 2001\n") && !output.ends_with("Line 2001"));
    assert!(output.contains("[Showing lines 1-2000 of 2500. Use offset=2001 to continue.]"));

    let truncation = result.details.unwrap().truncation.unwrap();
    assert!(truncation.truncated);
    assert_eq!(truncation.truncated_by, Some("lines"));
    assert_eq!(truncation.total_lines, 2500);
    assert_eq!(truncation.output_lines, 2000);
}

#[test]
fn read_truncates_byte_limit() {
    let dir = tmp_dir();
    let lines: Vec<String> = (1..=500)
        .map(|i| format!("Line {i}: {}", "x".repeat(200)))
        .collect();
    let test_file = write_file(dir.path(), "large-bytes.txt", lines.join("\n"));

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file }),
    )
    .unwrap();
    let output = text_output(&result);

    assert!(output.contains("Line 1:"));
    assert!(
        output.contains(" limit). Use offset=") && output.contains("[Showing lines 1-"),
        "missing byte-limit message: {}",
        &output[output.len().saturating_sub(200)..]
    );
}

#[test]
fn read_offset() {
    let dir = tmp_dir();
    let lines: Vec<String> = (1..=100).map(|i| format!("Line {i}")).collect();
    let test_file = write_file(dir.path(), "offset-test.txt", lines.join("\n"));

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "offset": 51 }),
    )
    .unwrap();
    let output = text_output(&result);

    assert!(!output.contains("Line 50\n") && !output.starts_with("Line 50"));
    assert!(output.contains("Line 51"));
    assert!(output.contains("Line 100"));
    assert!(!output.contains("Use offset="));
}

#[test]
fn read_limit() {
    let dir = tmp_dir();
    let lines: Vec<String> = (1..=100).map(|i| format!("Line {i}")).collect();
    let test_file = write_file(dir.path(), "limit-test.txt", lines.join("\n"));

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "limit": 10 }),
    )
    .unwrap();
    let output = text_output(&result);

    assert!(output.contains("Line 1"));
    assert!(output.contains("Line 10"));
    assert!(!output.contains("Line 11\n") && !output.contains("Line 11\u{0}"));
    assert!(output.contains("[90 more lines in file. Use offset=11 to continue.]"));
}

#[test]
fn read_offset_and_limit() {
    let dir = tmp_dir();
    let lines: Vec<String> = (1..=100).map(|i| format!("Line {i}")).collect();
    let test_file = write_file(dir.path(), "offset-limit-test.txt", lines.join("\n"));

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "offset": 41, "limit": 20 }),
    )
    .unwrap();
    let output = text_output(&result);

    assert!(!output.starts_with("Line 40\n"));
    assert!(output.contains("Line 41"));
    assert!(output.contains("Line 60"));
    assert!(!output.contains("Line 61"));
    assert!(output.contains("[40 more lines in file. Use offset=61 to continue.]"));
}

#[test]
fn read_offset_beyond_eof() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "short.txt", "Line 1\nLine 2\nLine 3");

    let err = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "offset": 100 }),
    )
    .unwrap_err();
    assert_eq!(err.0, "Offset 100 is beyond end of file (3 lines total)");
}

#[test]
fn read_detects_image_mime_by_magic() {
    let png_1x1_base64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg==";
    let png_buffer = base64::engine::general_purpose::STANDARD
        .decode(png_1x1_base64)
        .unwrap();

    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "image.txt", &png_buffer);

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file }),
    )
    .unwrap();

    assert!(matches!(result.content[0], ContentBlock::Text { .. }));
    assert!(text_output(&result).contains("Read image file [image/png]"));
    let (data, mime) = image_block(&result).expect("image block present");
    assert_eq!(mime, "image/png");
    assert!(!data.is_empty());
}

#[test]
fn read_bmp_as_png_attachment() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "image.bmp", create_tiny_bmp_1x1_red_24bpp());

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file }),
    )
    .unwrap();

    assert!(text_output(&result).contains("Read image file [image/png]"));
    assert!(text_output(&result).contains("[Image converted from image/bmp to image/png.]"));
    let (data, mime) = image_block(&result).expect("image block present");
    assert_eq!(mime, "image/png");
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(data)
        .unwrap();
    assert_eq!(decoded[0], 0x89);
}

#[test]
fn read_image_extension_with_text_content() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "not-an-image.png", "definitely not a png");

    let result = read_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file }),
    )
    .unwrap();
    assert!(text_output(&result).contains("definitely not a png"));
    assert!(image_block(&result).is_none());
}

// --------------------------------------------------------------- write tool

#[test]
fn write_file_contents() {
    let dir = tmp_dir();
    let test_file = dir.path().join("write-test.txt");

    let parsed: tf_harness_tools::write::WriteArgs = serde_json::from_value(serde_json::json!({
        "path": test_file.to_str().unwrap(),
        "content": "Test content",
    }))
    .unwrap();
    let result = tf_harness_tools::write::execute(dir.path().to_str().unwrap(), &parsed).unwrap();

    assert!(text_output(&result).contains("Successfully wrote"));
    assert!(text_output(&result).contains(test_file.to_str().unwrap()));
    assert_eq!(
        text_output(&result),
        format!(
            "Successfully wrote 12 bytes to {}",
            test_file.to_str().unwrap()
        )
    );
    assert_eq!(std::fs::read_to_string(&test_file).unwrap(), "Test content");
}

#[test]
fn write_creates_parent_directories() {
    let dir = tmp_dir();
    let test_file = dir.path().join("nested").join("dir").join("test.txt");

    let parsed: tf_harness_tools::write::WriteArgs = serde_json::from_value(serde_json::json!({
        "path": test_file.to_str().unwrap(),
        "content": "Nested content",
    }))
    .unwrap();
    let result = tf_harness_tools::write::execute(dir.path().to_str().unwrap(), &parsed).unwrap();

    assert!(text_output(&result).contains("Successfully wrote"));
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "Nested content"
    );
}

// ---------------------------------------------------------------- bash tool

#[test]
fn bash_simple_command() {
    let dir = tmp_dir();
    let result = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "echo 'test output'" }),
    )
    .unwrap();
    assert!(text_output(&result).contains("test output"));
    assert!(result.details.is_none());
}

#[test]
fn bash_command_error() {
    let dir = tmp_dir();
    let err = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "exit 1" }),
    )
    .unwrap_err();
    // pi feeds appendStatus the "(no output)" default, so the empty-output
    // error reads exactly like this.
    assert_eq!(err.0, "(no output)\n\nCommand exited with code 1");
}

#[test]
fn bash_timeout() {
    let dir = tmp_dir();
    let err = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "sleep 5", "timeout": 1 }),
    )
    .unwrap_err();
    assert!(
        err.0.to_lowercase().contains("timed out"),
        "unexpected: {}",
        err.0
    );
    assert_eq!(err.0, "Command timed out after 1 seconds");
}

#[test]
fn bash_truncated_timeout_includes_full_output_path() {
    let dir = tmp_dir();
    let err = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "seq 3000; sleep 5", "timeout": 2 }),
    )
    .unwrap_err();
    assert!(
        err.0.contains("Command timed out after 2 seconds"),
        "unexpected: {}",
        err.0
    );
    assert!(err.0.contains("[Showing lines "), "unexpected: {}", err.0);
    assert!(
        !err.0.contains("Full output: undefined"),
        "unexpected: {}",
        err.0
    );
    let path = err
        .0
        .split("Full output: ")
        .nth(1)
        .and_then(|s| s.split(']').next())
        .expect("full output path");
    let full_output = std::fs::read_to_string(path).unwrap();
    assert!(full_output.contains("1\n2\n3"));
    assert!(full_output.contains("2998\n2999\n3000"));
    let _ = std::fs::remove_file(path);
}

#[test]
fn bash_nonexistent_cwd() {
    let err = bash_tool(
        "/this/directory/definitely/does/not/exist/12345",
        serde_json::json!({ "command": "echo test" }),
    )
    .unwrap_err();
    assert!(
        err.0.contains("Working directory does not exist"),
        "unexpected: {}",
        err.0
    );
}

#[test]
fn bash_custom_shell_path_not_found() {
    let dir = tmp_dir();
    let parsed: tf_harness_tools::bash::BashArgs =
        serde_json::from_value(serde_json::json!({ "command": "echo test" })).unwrap();
    let err = tf_harness_tools::bash::execute(
        dir.path().to_str().unwrap(),
        &parsed,
        &tf_harness_tools::bash::BashOptions {
            shell_path: Some("/custom/bash".to_string()),
            ..Default::default()
        },
    )
    .unwrap_err();
    assert_eq!(err.0, "Custom shell path not found: /custom/bash");
}

#[test]
fn bash_command_prefix() {
    let dir = tmp_dir();
    let cwd = dir.path().to_str().unwrap();
    let parsed: tf_harness_tools::bash::BashArgs =
        serde_json::from_value(serde_json::json!({ "command": "echo $TEST_VAR" })).unwrap();
    let opts = tf_harness_tools::bash::BashOptions {
        command_prefix: Some("export TEST_VAR=hello".to_string()),
        ..Default::default()
    };
    let result = tf_harness_tools::bash::execute(cwd, &parsed, &opts).unwrap();
    assert_eq!(text_output(&result).trim(), "hello");

    let parsed: tf_harness_tools::bash::BashArgs =
        serde_json::from_value(serde_json::json!({ "command": "echo command-output" })).unwrap();
    let opts = tf_harness_tools::bash::BashOptions {
        command_prefix: Some("echo prefix-output".to_string()),
        ..Default::default()
    };
    let result = tf_harness_tools::bash::execute(cwd, &parsed, &opts).unwrap();
    assert_eq!(text_output(&result).trim(), "prefix-output\ncommand-output");

    let parsed: tf_harness_tools::bash::BashArgs =
        serde_json::from_value(serde_json::json!({ "command": "echo no-prefix" })).unwrap();
    let result = tf_harness_tools::bash::execute(
        cwd,
        &parsed,
        &tf_harness_tools::bash::BashOptions::default(),
    )
    .unwrap();
    assert_eq!(text_output(&result).trim(), "no-prefix");
}

#[test]
fn bash_trailing_newline_not_extra_line() {
    let dir = tmp_dir();
    let result = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "seq -f 'line-%04.0f' 4000" }),
    )
    .unwrap();
    let output = text_output(&result);
    let details = result.details.as_ref().unwrap();
    let truncation = details.truncation.as_ref().unwrap();

    assert_eq!(truncation.total_lines, 4000);
    assert_eq!(truncation.output_lines, 2000);
    assert!(output.contains("line-2001"));
    assert!(output.contains("line-4000"));
    assert!(output.contains("[Showing lines 2001-4000 of 4000. Full output: "));
    assert!(!output.contains("4001"));
    if let Some(path) = &details.full_output_path {
        let _ = std::fs::remove_file(path);
    }
}

#[test]
fn bash_utf8_split_across_chunks() {
    let dir = tmp_dir();
    let result = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "printf '\\xe2\\x82'; sleep 0.2; printf '\\xac\\n'" }),
    )
    .unwrap();
    assert_eq!(text_output(&result).trim(), "€");
}

#[test]
fn bash_no_output_label() {
    let dir = tmp_dir();
    let result = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "true" }),
    )
    .unwrap();
    assert_eq!(text_output(&result), "(no output)");
}

#[test]
fn bash_line_truncation_persists_full_output() {
    let dir = tmp_dir();
    let result = bash_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "command": "seq 3000" }),
    )
    .unwrap();
    let output = text_output(&result);
    let details = result.details.as_ref().unwrap();
    let truncation = details.truncation.as_ref().unwrap();

    assert!(truncation.truncated);
    assert_eq!(truncation.truncated_by, Some("lines"));
    let full_output_path = details.full_output_path.as_ref().expect("full output path");
    assert!(output.contains("[Showing lines "));
    assert!(!output.contains("Full output: undefined"));
    let full_output = std::fs::read_to_string(full_output_path).unwrap();
    assert!(full_output.contains("1\n2\n3"));
    assert!(full_output.contains("2998\n2999\n3000"));
    let _ = std::fs::remove_file(full_output_path);
}

// ---------------------------------------------------------------- grep tool

#[test]
fn grep_single_file_includes_filename() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "example.txt",
        "first line\nmatch line\nlast line",
    );

    let result = grep_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "match", "path": test_file }),
    )
    .unwrap();
    assert!(text_output(&result).contains("example.txt:2: match line"));
}

#[test]
fn grep_limit_and_context() {
    let dir = tmp_dir();
    let content = [
        "before",
        "match one",
        "after",
        "middle",
        "match two",
        "after two",
    ]
    .join("\n");
    let test_file = write_file(dir.path(), "context.txt", content);

    let result = grep_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "match", "path": test_file, "limit": 1, "context": 1 }),
    )
    .unwrap();
    let output = text_output(&result);
    assert!(output.contains("context.txt-1- before"), "got: {output}");
    assert!(output.contains("context.txt:2: match one"), "got: {output}");
    assert!(output.contains("context.txt-3- after"), "got: {output}");
    assert!(
        output.contains("[1 matches limit reached. Use limit=2 for more, or refine pattern]"),
        "got: {output}"
    );
    assert!(!output.contains("match two"));
}

#[test]
fn grep_flag_like_pattern_is_search_text() {
    let dir = tmp_dir();
    write_file(dir.path(), "target.txt", "target\n");

    let result = grep_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "--pre=/tmp/payload.sh", "path": dir.path().to_str().unwrap() }),
    )
    .unwrap();
    assert!(text_output(&result).contains("No matches found"));
}

#[test]
fn grep_no_matches() {
    let dir = tmp_dir();
    write_file(dir.path(), "a.txt", "nothing here\n");
    let result = grep_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "zzz-does-not-exist", "path": dir.path().to_str().unwrap() }),
    )
    .unwrap();
    assert_eq!(text_output(&result), "No matches found");
    assert!(result.details.is_none());
}

#[test]
fn grep_path_not_found() {
    let dir = tmp_dir();
    let missing = dir.path().join("missing-subdir");
    let err = grep_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "x", "path": missing.to_str().unwrap() }),
    )
    .unwrap_err();
    assert_eq!(
        err.0,
        format!("Path not found: {}", missing.to_str().unwrap())
    );
}

// ---------------------------------------------------------------- find tool

#[test]
fn find_includes_hidden_not_gitignored() {
    let dir = tmp_dir();
    std::fs::create_dir(dir.path().join(".secret")).unwrap();
    write_file(dir.path(), ".secret/hidden.txt", "hidden");
    write_file(dir.path(), "visible.txt", "visible");

    let result = find_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "**/*.txt", "path": dir.path().to_str().unwrap() }),
    )
    .unwrap();
    let output = text_output(&result);
    let lines: Vec<&str> = output
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .collect();
    assert!(lines.contains(&"visible.txt"), "got: {lines:?}");
    assert!(lines.contains(&".secret/hidden.txt"), "got: {lines:?}");
}

#[test]
fn find_respects_gitignore() {
    let dir = tmp_dir();
    write_file(dir.path(), ".gitignore", "ignored.txt\n");
    write_file(dir.path(), "ignored.txt", "ignored");
    write_file(dir.path(), "kept.txt", "kept");

    let result = find_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "**/*.txt", "path": dir.path().to_str().unwrap() }),
    )
    .unwrap();
    let output = text_output(&result);
    assert!(output.contains("kept.txt"));
    assert!(!output.contains("ignored.txt"), "got: {output}");
}

#[test]
fn find_glob_parse_error() {
    let dir = tmp_dir();
    let err = find_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "[", "path": dir.path().to_str().unwrap() }),
    )
    .unwrap_err();
    let lower = err.0.to_lowercase();
    assert!(
        lower.contains("error parsing glob")
            || lower.contains("fd exited with code 1")
            || lower.contains("fd error"),
        "unexpected: {}",
        err.0
    );
}

#[test]
fn find_flag_like_pattern_is_search_text() {
    let dir = tmp_dir();
    write_file(dir.path(), "a.txt", "x");
    let result = find_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "pattern": "--help", "path": dir.path().to_str().unwrap() }),
    )
    .unwrap();
    assert!(text_output(&result).contains("No files found matching pattern"));
}

// ------------------------------------------------------------------ ls tool

#[test]
fn ls_dotfiles_and_directories() {
    let dir = tmp_dir();
    write_file(dir.path(), ".hidden-file", "secret");
    std::fs::create_dir(dir.path().join(".hidden-dir")).unwrap();

    let result = ls_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": dir.path().to_str().unwrap() }),
    )
    .unwrap();
    let output = text_output(&result);
    assert!(output.contains(".hidden-file"));
    assert!(output.contains(".hidden-dir/"));
}

#[test]
fn ls_empty_directory() {
    let dir = tmp_dir();
    let result = ls_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": dir.path().to_str().unwrap() }),
    )
    .unwrap();
    assert_eq!(text_output(&result), "(empty directory)");
}

#[test]
fn ls_path_not_found_and_not_a_directory() {
    let dir = tmp_dir();
    let missing = dir.path().join("nope");
    let err = ls_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": missing.to_str().unwrap() }),
    )
    .unwrap_err();
    assert_eq!(
        err.0,
        format!("Path not found: {}", missing.to_str().unwrap())
    );

    let file = write_file(dir.path(), "plain.txt", "x");
    let err = ls_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": file }),
    )
    .unwrap_err();
    assert_eq!(err.0, format!("Not a directory: {file}"));
}

#[test]
fn ls_entry_limit() {
    let dir = tmp_dir();
    for i in 0..10 {
        write_file(dir.path(), &format!("f{i:02}.txt"), "x");
    }
    let result = ls_tool(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": dir.path().to_str().unwrap(), "limit": 5 }),
    )
    .unwrap();
    let output = text_output(&result);
    assert!(
        output.contains("[5 entries limit reached. Use limit=10 for more]"),
        "got: {output}"
    );
    assert_eq!(result.details.unwrap().entry_limit_reached, Some(5));
}
