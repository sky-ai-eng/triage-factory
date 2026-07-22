//! Port of pi's `utils/shell.ts` (POSIX paths only): shell resolution and
//! process-tree kill.

use crate::result::ToolError;

pub struct ShellConfig {
    pub shell: String,
    pub args: Vec<&'static str>,
}

/// In-process PATH walk with `which`'s semantics (first entry holding an
/// executable `bash`; an empty PATH segment means the current directory).
/// pi shells out to `which` here; a fork/exec — and a dependency on `which`
/// existing in minimal images — has no place in this harness, and this
/// codepath only runs at all when /bin/bash is absent.
fn find_bash_on_path() -> Option<String> {
    let path_var = std::env::var("PATH").ok()?;
    for dir in path_var.split(':') {
        let dir = if dir.is_empty() { "." } else { dir };
        let candidate = format!("{dir}/bash");
        let Ok(c_path) = std::ffi::CString::new(candidate.as_bytes()) else {
            continue;
        };
        if unsafe { libc::access(c_path.as_ptr(), libc::X_OK) } == 0 {
            return Some(candidate);
        }
    }
    None
}

/// Resolution order: explicit shell path, /bin/bash, bash on PATH, sh.
pub fn get_shell_config(custom_shell_path: Option<&str>) -> Result<ShellConfig, ToolError> {
    if let Some(custom) = custom_shell_path {
        if std::path::Path::new(custom).exists() {
            return Ok(ShellConfig {
                shell: custom.to_string(),
                args: vec!["-c"],
            });
        }
        return Err(ToolError::new(format!(
            "Custom shell path not found: {custom}"
        )));
    }

    if std::path::Path::new("/bin/bash").exists() {
        return Ok(ShellConfig {
            shell: "/bin/bash".to_string(),
            args: vec!["-c"],
        });
    }
    if let Some(bash) = find_bash_on_path() {
        return Ok(ShellConfig {
            shell: bash,
            args: vec!["-c"],
        });
    }
    Ok(ShellConfig {
        shell: "sh".to_string(),
        args: vec!["-c"],
    })
}

/// SIGKILL the process group, falling back to the process itself.
pub fn kill_process_tree(pid: i32) {
    unsafe {
        if libc::kill(-pid, libc::SIGKILL) != 0 {
            libc::kill(pid, libc::SIGKILL);
        }
    }
}
