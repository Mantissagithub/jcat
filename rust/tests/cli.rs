use std::fs;
use std::io::Write;
use std::process::{Command, Stdio};

fn jcat() -> Command {
    Command::new(env!("CARGO_BIN_EXE_jcat"))
}

#[test]
fn formats_records_and_tracks_physical_lines() {
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("input.jsonl");
    fs::write(&file, b"{\"z\":1,\"a\":true}\r\n  \r\n[\"x\",null]").unwrap();
    let result = jcat()
        .args(["--paging", "never", "--color", "never"])
        .arg(file)
        .output()
        .unwrap();
    assert!(result.status.success());
    let output = String::from_utf8(result.stdout).unwrap();
    assert!(output.contains("record 1 | source line 1"));
    assert!(output.contains("record 2 | source line 3"));
    assert!(output.find("\"z\"").unwrap() < output.find("\"a\"").unwrap());
    assert!(output.contains("\n\nrecord 2"));
}

#[test]
fn reports_malformed_source_line_with_failure_status() {
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("bad.jsonl");
    fs::write(&file, b"{}\n\n{bad}\n").unwrap();
    let result = jcat()
        .args(["--paging", "never"])
        .arg(file)
        .output()
        .unwrap();
    assert_eq!(result.status.code(), Some(1));
    assert!(String::from_utf8_lossy(&result.stderr).contains("physical line 3"));
}

#[test]
fn stdin_and_help_work() {
    let mut child = jcat()
        .args(["--paging", "never", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(b"{\"stdin\":true}\n")
        .unwrap();
    let result = child.wait_with_output().unwrap();
    assert!(result.status.success());
    assert!(String::from_utf8_lossy(&result.stdout).contains("stdin"));
    let help = jcat().arg("--help").output().unwrap();
    assert!(help.status.success());
    assert!(String::from_utf8_lossy(&help.stdout).contains("Usage: jcat"));
}

#[test]
fn color_modes_and_large_numbers() {
    let input = b"{\"n\":1234567890123456789012345678901234567890}\n";
    let plain = jcat()
        .args(["--paging", "never", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    let mut plain = plain;
    plain.stdin.take().unwrap().write_all(input).unwrap();
    let plain = plain.wait_with_output().unwrap();
    assert!(!plain.stdout.contains(&0x1b));
    assert!(
        String::from_utf8_lossy(&plain.stdout).contains("1234567890123456789012345678901234567890")
    );

    let mut colored = jcat()
        .args(["--paging", "never", "--color", "always", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .spawn()
        .unwrap();
    colored.stdin.take().unwrap().write_all(input).unwrap();
    let colored = colored.wait_with_output().unwrap();
    assert!(colored.stdout.contains(&0x1b));
}

#[cfg(unix)]
#[test]
fn early_pager_exit_is_clean() {
    use std::os::unix::fs::PermissionsExt;

    let dir = tempfile::tempdir().unwrap();
    let pager = dir.path().join("less");
    fs::write(&pager, "#!/bin/sh\nexit 0\n").unwrap();
    fs::set_permissions(&pager, fs::Permissions::from_mode(0o755)).unwrap();

    let input = dir.path().join("large.jsonl");
    fs::write(&input, format!("\"{}\"\n", "x".repeat(300_000))).unwrap();
    let result = jcat()
        .args(["--paging", "always"])
        .arg(input)
        .env("PATH", dir.path())
        .output()
        .unwrap();
    assert!(
        result.status.success(),
        "{}",
        String::from_utf8_lossy(&result.stderr)
    );
}

#[test]
fn interactive_flag_requires_terminal_but_plain_keeps_cli() {
    let dir = tempfile::tempdir().unwrap();
    let file = dir.path().join("x.jsonl");
    fs::write(&file, b"{\"x\":1}\n").unwrap();
    let interactive = jcat().arg("--interactive").arg(&file).output().unwrap();
    assert_eq!(interactive.status.code(), Some(1));
    assert!(
        String::from_utf8_lossy(&interactive.stderr)
            .contains("requires stdin and stdout to be terminals")
    );
    let plain = jcat().arg("--plain").arg(&file).output().unwrap();
    assert!(plain.status.success());
    assert!(String::from_utf8_lossy(&plain.stdout).contains("record 1"));
}
