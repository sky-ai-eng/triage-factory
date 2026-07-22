//! Port of pi's `utils/shell.ts` (POSIX paths only): shell resolution and
//! process-tree kill.

use crate::result::ToolError;

pub struct ShellConfig {
    pub shell: String,
    pub args: Vec<&'static str>,
}

fn find_bash_on_path() -> Option<String> {
    let output = std::process::Command::new("which")
        .arg("bash")
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let stdout = String::from_utf8_lossy(&output.stdout);
    stdout
        .trim()
        .lines()
        .next()
        .map(str::to_string)
        .filter(|s| !s.is_empty())
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
