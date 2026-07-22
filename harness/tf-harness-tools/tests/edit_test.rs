//! Port of pi's edit-tool tests: the main suite from `test/tools.test.ts`,
//! the fuzzy-matching suite, the CRLF/BOM suite, and
//! `test/edit-tool-legacy-input.test.ts`. Expected strings are verbatim.

use tf_harness_tools::edit::{execute, parse_args, prepare_arguments};
use tf_harness_tools::result::{EditToolDetails, ToolError, ToolResult};

fn tmp_dir() -> tempfile::TempDir {
    tempfile::TempDir::new().unwrap()
}

fn write_file(dir: &std::path::Path, name: &str, content: impl AsRef<[u8]>) -> String {
    let path = dir.join(name);
    std::fs::write(&path, content.as_ref()).unwrap();
    path.to_string_lossy().into_owned()
}

fn edit(cwd: &str, args: serde_json::Value) -> Result<ToolResult<EditToolDetails>, ToolError> {
    let prepared = prepare_arguments(args);
    let parsed = parse_args(&prepared)?;
    execute(cwd, &parsed)
}

#[test]
fn edit_replaces_text() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-test.txt", "Hello, world!");

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "world", "newText": "testing" }] }),
    )
    .unwrap();

    assert!(result.text_output().contains("Successfully replaced"));
    let details = result.details.unwrap();
    assert!(details.diff.contains("testing"));
    assert!(details.patch.contains("--- "));
    assert!(details.patch.contains("+++ "));
    assert!(details.patch.contains("@@"));
    assert!(details.patch.contains("-Hello, world!"));
    assert!(details.patch.contains("+Hello, testing!"));
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "Hello, testing!"
    );
}

#[test]
fn edit_text_not_found() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-test.txt", "Hello, world!");

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "nonexistent", "newText": "testing" }] }),
    )
    .unwrap_err();
    assert!(
        err.0.contains("Could not find the exact text"),
        "got: {}",
        err.0
    );
}

#[test]
fn edit_missing_file_enoent() {
    let dir = tmp_dir();
    let missing_file = dir.path().join("missing.txt");

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": missing_file.to_str().unwrap(), "edits": [{ "oldText": "hello", "newText": "world" }] }),
    )
    .unwrap_err();
    assert_eq!(
        err.0,
        format!(
            "Could not edit file: {}. Error code: ENOENT.",
            missing_file.to_str().unwrap()
        )
    );
}

#[test]
fn edit_duplicate_text() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-test.txt", "foo foo foo");

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "foo", "newText": "bar" }] }),
    )
    .unwrap_err();
    assert!(err.0.contains("Found 3 occurrences"), "got: {}", err.0);
}

#[test]
fn edit_multiple_disjoint_regions() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-multi.txt", "alpha\nbeta\ngamma\ndelta\n");

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "alpha\n", "newText": "ALPHA\n" },
            { "oldText": "gamma\n", "newText": "GAMMA\n" },
        ] }),
    )
    .unwrap();

    assert!(result
        .text_output()
        .contains("Successfully replaced 2 block(s)"));
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "ALPHA\nbeta\nGAMMA\ndelta\n"
    );
    let details = result.details.unwrap();
    assert!(details.diff.contains("ALPHA"));
    assert!(details.diff.contains("GAMMA"));
}

#[test]
fn edit_collapses_large_unchanged_gaps() {
    let dir = tmp_dir();
    let lines: Vec<String> = (1..=600).map(|i| format!("line {i:03}")).collect();
    let test_file = write_file(
        dir.path(),
        "edit-multi-large-gap.txt",
        format!("{}\n", lines.join("\n")),
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "line 100\n", "newText": "LINE 100\n" },
            { "oldText": "line 300\n", "newText": "LINE 300\n" },
            { "oldText": "line 500\n", "newText": "LINE 500\n" },
        ] }),
    )
    .unwrap();

    let diff = result.details.unwrap().diff;
    assert!(diff.contains("LINE 100"));
    assert!(diff.contains("LINE 300"));
    assert!(diff.contains("LINE 500"));
    assert!(diff.contains("..."));
    assert!(!diff.contains("line 250"));
    assert!(diff.split('\n').count() < 50, "diff too long:\n{diff}");
}

#[test]
fn edit_matches_against_original_not_incrementally() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-multi-original.txt", "foo\nbar\nbaz\n");

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "foo\n", "newText": "foo bar\n" },
            { "oldText": "bar\n", "newText": "BAR\n" },
        ] }),
    )
    .unwrap();

    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "foo bar\nBAR\nbaz\n"
    );
}

#[test]
fn edit_empty_edits() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-empty-edits.txt", "hello\nworld\n");

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [] }),
    )
    .unwrap_err();
    assert!(
        err.0
            .contains("edits must contain at least one replacement"),
        "got: {}",
        err.0
    );
}

#[test]
fn edit_overlapping_regions() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-overlap.txt", "one\ntwo\nthree\n");

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "one\ntwo\n", "newText": "ONE\nTWO\n" },
            { "oldText": "two\nthree\n", "newText": "TWO\nTHREE\n" },
        ] }),
    )
    .unwrap_err();
    assert!(err.0.contains("overlap"), "got: {}", err.0);
}

#[test]
fn edit_no_partial_application() {
    let dir = tmp_dir();
    let original = "alpha\nbeta\ngamma\n";
    let test_file = write_file(dir.path(), "edit-no-partial.txt", original);

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "alpha\n", "newText": "ALPHA\n" },
            { "oldText": "missing\n", "newText": "MISSING\n" },
        ] }),
    )
    .unwrap_err();
    assert!(err.0.contains("Could not find"), "got: {}", err.0);
    assert_eq!(std::fs::read_to_string(&test_file).unwrap(), original);
}

#[test]
fn edit_readonly_eacces() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "edit-readonly.txt", "hello\n");
    let mut perms = std::fs::metadata(&test_file).unwrap().permissions();
    use std::os::unix::fs::PermissionsExt;
    perms.set_mode(0o444);
    std::fs::set_permissions(&test_file, perms).unwrap();

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "hello", "newText": "world" }] }),
    )
    .unwrap_err();
    assert_eq!(
        err.0,
        format!("Could not edit file: {test_file}. Error code: EACCES.")
    );
}

// ------------------------------------------------------------ fuzzy matching

#[test]
fn fuzzy_trailing_whitespace() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "trailing-ws.txt",
        "line one   \nline two  \nline three\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "line one\nline two\n", "newText": "replaced\n" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "replaced\nline three\n"
    );
}

#[test]
fn fuzzy_fullwidth_chinese_punctuation() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "chinese-punctuation.txt",
        "你好，世界\n你好（世界）\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "你好,世界\n你好(世界)\n", "newText": "你好，pi\n你好(pi)\n" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "你好，pi\n你好(pi)\n"
    );
}

#[test]
fn fuzzy_unicode_compatibility_forms() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "unicode-compatibility.txt",
        "ＡＢＣ１２３\ncafe\u{0301}\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "ABC123\ncafé\n", "newText": "XYZ789\ncoffee\n" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "XYZ789\ncoffee\n"
    );
}

#[test]
fn fuzzy_smart_single_quotes() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "smart-quotes.txt",
        "console.log(\u{2018}hello\u{2019});\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "console.log('hello');", "newText": "console.log('world');" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert!(std::fs::read_to_string(&test_file)
        .unwrap()
        .contains("world"));
}

#[test]
fn fuzzy_smart_double_quotes() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "smart-double-quotes.txt",
        "const msg = \u{201C}Hello World\u{201D};\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "const msg = \"Hello World\";", "newText": "const msg = \"Goodbye\";" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert!(std::fs::read_to_string(&test_file)
        .unwrap()
        .contains("Goodbye"));
}

#[test]
fn fuzzy_unicode_dashes() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "unicode-dashes.txt",
        "range: 1\u{2013}5\nbreak\u{2014}here\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "range: 1-5\nbreak-here", "newText": "range: 10-50\nbreak--here" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert!(std::fs::read_to_string(&test_file)
        .unwrap()
        .contains("10-50"));
}

#[test]
fn fuzzy_nbsp() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "nbsp.txt", "hello\u{00A0}world\n");

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "hello world", "newText": "hello universe" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert!(std::fs::read_to_string(&test_file)
        .unwrap()
        .contains("universe"));
}

#[test]
fn fuzzy_exact_match_preferred() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "exact-preferred.txt",
        "const x = 'exact';\nconst y = 'other';\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "const x = 'exact';", "newText": "const x = 'changed';" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "const x = 'changed';\nconst y = 'other';\n"
    );
}

#[test]
fn fuzzy_still_fails_when_not_found() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "no-match.txt", "completely different content\n");

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "this does not exist", "newText": "replacement" }] }),
    )
    .unwrap_err();
    assert!(
        err.0.contains("Could not find the exact text"),
        "got: {}",
        err.0
    );
}

#[test]
fn fuzzy_duplicates_after_normalization() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "fuzzy-dups.txt",
        "hello world   \nhello world\n",
    );

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "hello world", "newText": "replaced" }] }),
    )
    .unwrap_err();
    assert!(err.0.contains("Found 2 occurrences"), "got: {}", err.0);
}

#[test]
fn fuzzy_multi_edit() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "fuzzy-multi.txt",
        "console.log(\u{2018}hello\u{2019});\nhello\u{00A0}world\n",
    );

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "console.log('hello');\n", "newText": "console.log('world');\n" },
            { "oldText": "hello world\n", "newText": "hello universe\n" },
        ] }),
    )
    .unwrap();
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "console.log('world');\nhello universe\n"
    );
}

#[test]
fn fuzzy_preserves_correct_occurrence_with_duplicate_line() {
    let dir = tmp_dir();
    let original = "replace me   \nafter   \n";
    let test_file = write_file(dir.path(), "fuzzy-preserve-duplicate-line.txt", original);

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "replace me\n", "newText": "after\n" }] }),
    )
    .unwrap();

    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "after\nafter   \n"
    );
}

#[test]
fn fuzzy_preserves_untouched_lines_multi() {
    let dir = tmp_dir();
    let original = "keep before  \nfirst target  \nfirst after\nkeep middle   \nsecond target  \nsecond after\nkeep after  \n";
    let test_file = write_file(dir.path(), "fuzzy-preserve-multi.txt", original);

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "first target\nfirst after", "newText": "FIRST\nFIRST2" },
            { "oldText": "second target\nsecond after", "newText": "SECOND\nSECOND2" },
        ] }),
    )
    .unwrap();

    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "keep before  \nFIRST\nFIRST2\nkeep middle   \nSECOND\nSECOND2\nkeep after  \n"
    );
}

// ------------------------------------------------------------- CRLF handling

#[test]
fn crlf_lf_old_text_matches_crlf_file() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "crlf-test.txt",
        "line one\r\nline two\r\nline three\r\n",
    );

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "line two\n", "newText": "replaced line\n" }] }),
    )
    .unwrap();
    assert!(result.text_output().contains("Successfully replaced"));
}

#[test]
fn crlf_preserved_after_edit() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "crlf-preserve.txt",
        "first\r\nsecond\r\nthird\r\n",
    );

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "second\n", "newText": "REPLACED\n" }] }),
    )
    .unwrap();
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "first\r\nREPLACED\r\nthird\r\n"
    );
}

#[test]
fn lf_preserved_for_lf_files() {
    let dir = tmp_dir();
    let test_file = write_file(dir.path(), "lf-preserve.txt", "first\nsecond\nthird\n");

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "second\n", "newText": "REPLACED\n" }] }),
    )
    .unwrap();
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "first\nREPLACED\nthird\n"
    );
}

#[test]
fn crlf_duplicates_across_variants() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "mixed-endings.txt",
        "hello\r\nworld\r\n---\r\nhello\nworld\n",
    );

    let err = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "hello\nworld\n", "newText": "replaced\n" }] }),
    )
    .unwrap_err();
    assert!(err.0.contains("Found 2 occurrences"), "got: {}", err.0);
}

#[test]
fn bom_preserved_after_edit() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "bom-test.txt",
        "\u{FEFF}first\r\nsecond\r\nthird\r\n",
    );

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [{ "oldText": "second\n", "newText": "REPLACED\n" }] }),
    )
    .unwrap();
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "\u{FEFF}first\r\nREPLACED\r\nthird\r\n"
    );
}

#[test]
fn bom_crlf_multi_edit() {
    let dir = tmp_dir();
    let test_file = write_file(
        dir.path(),
        "bom-crlf-multi.txt",
        "\u{FEFF}first\r\nsecond\r\nthird\r\nfourth\r\n",
    );

    edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": test_file, "edits": [
            { "oldText": "second\n", "newText": "SECOND\n" },
            { "oldText": "fourth\n", "newText": "FOURTH\n" },
        ] }),
    )
    .unwrap();
    assert_eq!(
        std::fs::read_to_string(&test_file).unwrap(),
        "\u{FEFF}first\r\nSECOND\r\nthird\r\nFOURTH\r\n"
    );
}

// ------------------------------------------------------- prepareArguments

#[test]
fn prepare_folds_legacy_old_new_text() {
    let prepared = prepare_arguments(serde_json::json!({
        "path": "file.txt",
        "oldText": "before",
        "newText": "after",
    }));
    assert_eq!(
        prepared,
        serde_json::json!({
            "path": "file.txt",
            "edits": [{ "oldText": "before", "newText": "after" }],
        })
    );
}

#[test]
fn prepare_appends_legacy_to_existing_edits() {
    let prepared = prepare_arguments(serde_json::json!({
        "path": "file.txt",
        "edits": [{ "oldText": "a", "newText": "b" }],
        "oldText": "c",
        "newText": "d",
    }));
    assert_eq!(
        prepared,
        serde_json::json!({
            "path": "file.txt",
            "edits": [
                { "oldText": "a", "newText": "b" },
                { "oldText": "c", "newText": "d" },
            ],
        })
    );
}

#[test]
fn prepare_passes_valid_input_unchanged() {
    let input = serde_json::json!({
        "path": "file.txt",
        "edits": [{ "oldText": "a", "newText": "b" }],
    });
    assert_eq!(prepare_arguments(input.clone()), input);
}

#[test]
fn prepare_passes_non_object_unchanged() {
    assert_eq!(
        prepare_arguments(serde_json::Value::Null),
        serde_json::Value::Null
    );
    assert_eq!(
        prepare_arguments(serde_json::json!("garbage")),
        serde_json::json!("garbage")
    );
}

#[test]
fn prepare_parses_stringified_edits() {
    let prepared = prepare_arguments(serde_json::json!({
        "path": "file.txt",
        "edits": "[{\"oldText\":\"a\",\"newText\":\"b\"}]",
    }));
    assert_eq!(
        prepared,
        serde_json::json!({
            "path": "file.txt",
            "edits": [{ "oldText": "a", "newText": "b" }],
        })
    );
}

#[test]
fn prepare_leaves_invalid_json_string() {
    let prepared = prepare_arguments(serde_json::json!({
        "path": "file.txt",
        "edits": "not json",
    }));
    assert_eq!(
        prepared,
        serde_json::json!({
            "path": "file.txt",
            "edits": "not json",
        })
    );
}

#[test]
fn prepared_legacy_args_execute() {
    let dir = tmp_dir();
    write_file(dir.path(), "legacy.txt", "before\n");

    let result = edit(
        dir.path().to_str().unwrap(),
        serde_json::json!({ "path": "legacy.txt", "oldText": "before", "newText": "after" }),
    )
    .unwrap();
    assert_eq!(
        result.text_output(),
        "Successfully replaced 1 block(s) in legacy.txt."
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("legacy.txt")).unwrap(),
        "after\n"
    );
}
