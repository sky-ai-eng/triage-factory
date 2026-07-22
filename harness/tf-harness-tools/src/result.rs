//! Tool result shapes, serialized to match pi's tool contract.

use serde::Serialize;

use crate::truncate::TruncationResult;

#[derive(Debug, Clone, Serialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum ContentBlock {
    Text {
        text: String,
    },
    Image {
        data: String,
        #[serde(rename = "mimeType")]
        mime_type: String,
    },
}

impl ContentBlock {
    pub fn text(text: impl Into<String>) -> Self {
        ContentBlock::Text { text: text.into() }
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct ToolResult<D: Serialize> {
    pub content: Vec<ContentBlock>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub details: Option<D>,
}

impl<D: Serialize> ToolResult<D> {
    pub fn text_only(text: impl Into<String>) -> Self {
        ToolResult {
            content: vec![ContentBlock::text(text)],
            details: None,
        }
    }

    /// Concatenated text blocks, like the test helper in pi's tools.test.ts.
    pub fn text_output(&self) -> String {
        self.content
            .iter()
            .filter_map(|c| match c {
                ContentBlock::Text { text } => Some(text.as_str()),
                _ => None,
            })
            .collect::<Vec<_>>()
            .join("\n")
    }
}

/// A tool failure: pi throws `Error(message)`; the agent loop renders the
/// message as an error tool result.
#[derive(Debug)]
pub struct ToolError(pub String);

impl std::fmt::Display for ToolError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for ToolError {}

impl ToolError {
    pub fn new(message: impl Into<String>) -> Self {
        ToolError(message.into())
    }
}

pub type ToolResultOrError<D> = Result<ToolResult<D>, ToolError>;

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ReadToolDetails {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub truncation: Option<TruncationResult>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct BashToolDetails {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub truncation: Option<TruncationResult>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub full_output_path: Option<String>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct EditToolDetails {
    pub diff: String,
    pub patch: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub first_changed_line: Option<usize>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct GrepToolDetails {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub truncation: Option<TruncationResult>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub match_limit_reached: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub lines_truncated: Option<bool>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct FindToolDetails {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub truncation: Option<TruncationResult>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result_limit_reached: Option<usize>,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct LsToolDetails {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub truncation: Option<TruncationResult>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub entry_limit_reached: Option<usize>,
}

/// pi returns `details: undefined` when no details fields are set; mirror
/// that by collapsing empty details to None.
pub trait EmptyCheck {
    fn is_empty_details(&self) -> bool;
}

impl EmptyCheck for GrepToolDetails {
    fn is_empty_details(&self) -> bool {
        self.truncation.is_none()
            && self.match_limit_reached.is_none()
            && self.lines_truncated.is_none()
    }
}

impl EmptyCheck for FindToolDetails {
    fn is_empty_details(&self) -> bool {
        self.truncation.is_none() && self.result_limit_reached.is_none()
    }
}

impl EmptyCheck for LsToolDetails {
    fn is_empty_details(&self) -> bool {
        self.truncation.is_none() && self.entry_limit_reached.is_none()
    }
}
