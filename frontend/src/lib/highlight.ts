import { refractor } from "refractor/lib/core.js";

// Register languages we'll commonly see in code reviews.
// Import individually to avoid pulling in the full ~300-language bundle.
import go from "refractor/lang/go.js";
import typescript from "refractor/lang/typescript.js";
import javascript from "refractor/lang/javascript.js";
import tsx from "refractor/lang/tsx.js";
import jsx from "refractor/lang/jsx.js";
import python from "refractor/lang/python.js";
import rust from "refractor/lang/rust.js";
import java from "refractor/lang/java.js";
import kotlin from "refractor/lang/kotlin.js";
import swift from "refractor/lang/swift.js";
import css from "refractor/lang/css.js";
import scss from "refractor/lang/scss.js";
import json from "refractor/lang/json.js";
import yaml from "refractor/lang/yaml.js";
import toml from "refractor/lang/toml.js";
import bash from "refractor/lang/bash.js";
import sql from "refractor/lang/sql.js";
import markdown from "refractor/lang/markdown.js";
import docker from "refractor/lang/docker.js";
import protobuf from "refractor/lang/protobuf.js";
import makefile from "refractor/lang/makefile.js";
import ruby from "refractor/lang/ruby.js";
import csharp from "refractor/lang/csharp.js";
import cpp from "refractor/lang/cpp.js";
import c from "refractor/lang/c.js";

refractor.register(go);
refractor.register(typescript);
refractor.register(javascript);
refractor.register(tsx);
refractor.register(jsx);
refractor.register(python);
refractor.register(rust);
refractor.register(java);
refractor.register(kotlin);
refractor.register(swift);
refractor.register(css);
refractor.register(scss);
refractor.register(json);
refractor.register(yaml);
refractor.register(toml);
refractor.register(bash);
refractor.register(sql);
refractor.register(markdown);
refractor.register(docker);
refractor.register(protobuf);
refractor.register(makefile);
refractor.register(ruby);
refractor.register(csharp);
refractor.register(cpp);
refractor.register(c);

const EXT_MAP: Record<string, string> = {
  ".go": "go",
  ".ts": "typescript",
  ".tsx": "tsx",
  ".js": "javascript",
  ".jsx": "jsx",
  ".mjs": "javascript",
  ".cjs": "javascript",
  ".py": "python",
  ".rs": "rust",
  ".java": "java",
  ".kt": "kotlin",
  ".kts": "kotlin",
  ".swift": "swift",
  ".css": "css",
  ".scss": "scss",
  ".json": "json",
  ".yaml": "yaml",
  ".yml": "yaml",
  ".toml": "toml",
  ".sh": "bash",
  ".bash": "bash",
  ".zsh": "bash",
  ".sql": "sql",
  ".md": "markdown",
  ".mdx": "markdown",
  ".dockerfile": "docker",
  ".proto": "protobuf",
  ".rb": "ruby",
  ".cs": "csharp",
  ".cpp": "cpp",
  ".cc": "cpp",
  ".cxx": "cpp",
  ".h": "c",
  ".c": "c",
  ".hpp": "cpp",
  ".makefile": "makefile",
};

// Special filename matches (no extension)
const NAME_MAP: Record<string, string> = {
  Dockerfile: "docker",
  Makefile: "makefile",
  Jenkinsfile: "groovy",
};

/** Returns the refractor language name for a file path, or null if unknown. */
export function languageForPath(path: string): string | null {
  // Check full filename first
  const name = path.split("/").pop() ?? "";
  if (NAME_MAP[name]) return NAME_MAP[name];

  // Check extension
  const dot = name.lastIndexOf(".");
  if (dot >= 0) {
    const ext = name.slice(dot).toLowerCase();
    if (EXT_MAP[ext]) return EXT_MAP[ext];
  }

  return null;
}

export { refractor };
