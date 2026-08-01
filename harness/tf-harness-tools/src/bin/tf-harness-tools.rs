//! CLI for exercising the tools standalone and for the differential parity
//! harness.
//!
//! Usage:
//!   tf-harness-tools <tool> [--cwd DIR] [ARGS_JSON]
//!   tf-harness-tools serve --connect PATH [--cwd DIR]
//!   tf-harness-tools --definitions
//!
//! Args JSON comes from the positional argument or stdin. Output is a JSON
//! envelope on stdout: {"ok":true,"result":{...}} or
//! {"ok":false,"error":"..."} (exit 1).
//!
//! `serve` is the resident tool-host: it connects to a unix stream socket the
//! loop has already bound and answers length-prefixed JSON tool-call frames on
//! it. See `tf_harness_tools::serve` for the protocol and for why this side
//! dials rather than listens.

use std::io::Read;
use std::path::PathBuf;

fn main() {
    let argv: Vec<String> = std::env::args().collect();

    if argv.get(1).map(String::as_str) == Some("--definitions") {
        println!(
            "{}",
            serde_json::to_string_pretty(&tf_harness_tools::definitions::all_tool_definitions())
                .unwrap()
        );
        return;
    }

    if argv.get(1).map(String::as_str) == Some("serve") {
        run_serve(&argv[2..]);
        return;
    }

    let Some(tool) = argv.get(1) else {
        eprintln!("usage: tf-harness-tools <tool> [--cwd DIR] [ARGS_JSON] | tf-harness-tools serve --connect PATH [--cwd DIR] | tf-harness-tools --definitions");
        std::process::exit(2);
    };

    let mut cwd: Option<String> = None;
    let mut args_json: Option<String> = None;
    let mut repeat: usize = 1;
    let mut i = 2;
    while i < argv.len() {
        match argv[i].as_str() {
            "--cwd" => {
                cwd = argv.get(i + 1).cloned();
                i += 2;
            }
            "--repeat" => {
                repeat = argv.get(i + 1).and_then(|s| s.parse().ok()).unwrap_or(1);
                i += 2;
            }
            other => {
                args_json = Some(other.to_string());
                i += 1;
            }
        }
    }

    let cwd = cwd.unwrap_or_else(|| {
        std::env::current_dir()
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_else(|_| "/".to_string())
    });

    let args_json = args_json.unwrap_or_else(|| {
        let mut buf = String::new();
        let _ = std::io::stdin().read_to_string(&mut buf);
        buf
    });

    let args: serde_json::Value = match serde_json::from_str(&args_json) {
        Ok(v) => v,
        Err(e) => {
            println!(
                "{}",
                serde_json::json!({ "ok": false, "error": format!("Invalid args JSON: {e}") })
            );
            std::process::exit(2);
        }
    };

    // --repeat exists for marginal-syscall benchmarking; only the final
    // envelope is printed.
    for _ in 1..repeat {
        let _ = tf_harness_tools::run_tool(tool, &cwd, args.clone());
    }
    match tf_harness_tools::run_tool(tool, &cwd, args) {
        Ok(result) => {
            println!("{}", serde_json::json!({ "ok": true, "result": result }));
        }
        Err(err) => {
            println!("{}", serde_json::json!({ "ok": false, "error": err.0 }));
            std::process::exit(1);
        }
    }
}

/// `serve --connect PATH [--cwd DIR]`: dial the peer's socket and serve one
/// engagement over it. `--cwd` defaults to the process working directory,
/// matching the one-shot path.
///
/// The flag names the direction because the direction is the surprising part:
/// the peer binds and this process connects. See the `serve` module header —
/// a listener in here would need gVisor's `--host-uds=create`, which the jail
/// deliberately does not get.
fn run_serve(args: &[String]) {
    let mut socket: Option<PathBuf> = None;
    let mut cwd: Option<String> = None;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--connect" => {
                socket = args.get(i + 1).map(PathBuf::from);
                i += 2;
            }
            "--cwd" => {
                cwd = args.get(i + 1).cloned();
                i += 2;
            }
            other => {
                eprintln!("serve: unexpected argument: {other}");
                std::process::exit(2);
            }
        }
    }

    let Some(socket) = socket else {
        eprintln!("serve: --connect PATH is required");
        std::process::exit(2);
    };

    let cwd = cwd.unwrap_or_else(|| {
        std::env::current_dir()
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_else(|_| "/".to_string())
    });

    if let Err(err) = tf_harness_tools::serve::connect_and_serve(&socket, &cwd) {
        eprintln!("serve: {err}");
        std::process::exit(1);
    }
}
