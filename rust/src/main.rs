mod tui;

use std::env;
use std::fs::File;
use std::io::{self, BufRead, BufReader, BufWriter, IsTerminal, Read, Write};
use std::process::{Child, Command, Stdio};

#[derive(Clone, Copy, PartialEq)]
enum Mode {
    Auto,
    Always,
    Never,
}

struct Options {
    input: String,
    paging: Mode,
    color: Mode,
    plain: bool,
    interactive: bool,
}

fn usage() -> &'static str {
    "Usage: jcat [--plain|--interactive] [--paging auto|always|never] [--color auto|always|never] [FILE|-]\n\nPretty-print JSONL records with record and source-line numbers. Reads stdin when FILE is omitted or '-'."
}

fn parse_args() -> Result<Option<Options>, String> {
    let mut input = None;
    let mut options_ended = false;
    let mut paging = Mode::Auto;
    let mut color = Mode::Auto;
    let mut plain = false;
    let mut interactive = false;
    let mut args = env::args().skip(1);
    while let Some(arg) = args.next() {
        if options_ended {
            if input.is_some() {
                return Err("only one input file may be specified".into());
            }
            input = Some(arg);
            continue;
        }
        match arg.as_str() {
            "--" => options_ended = true,
            "--help" | "-h" => return Ok(None),
            "--plain" => plain = true,
            "--interactive" => interactive = true,
            "--paging" | "--color" => {
                let value = args
                    .next()
                    .ok_or_else(|| format!("{arg} requires auto, always, or never"))?;
                let mode = match value.as_str() {
                    "auto" => Mode::Auto,
                    "always" => Mode::Always,
                    "never" => Mode::Never,
                    _ => return Err(format!("invalid value for {arg}: {value}")),
                };
                if arg == "--paging" {
                    paging = mode
                } else {
                    color = mode
                }
            }
            _ if arg.starts_with('-') && arg != "-" => {
                return Err(format!("unknown option: {arg}"));
            }
            _ if input.is_some() => return Err("only one input file may be specified".into()),
            _ => input = Some(arg),
        }
    }
    if plain && interactive {
        return Err("--plain and --interactive cannot be used together".into());
    }
    Ok(Some(Options {
        input: input.unwrap_or_else(|| "-".into()),
        paging,
        color,
        plain,
        interactive,
    }))
}

struct Output {
    child: Option<Child>,
    writer: BufWriter<Box<dyn Write>>,
}

impl Output {
    fn new(paging: bool) -> io::Result<Self> {
        if paging {
            let mut child = Command::new("less")
                .args(["-R", "-F", "-X", "-L", "--"])
                .stdin(Stdio::piped())
                .spawn()?;
            let writer = child.stdin.take().expect("piped less stdin");
            Ok(Self {
                child: Some(child),
                writer: BufWriter::new(Box::new(writer)),
            })
        } else {
            Ok(Self {
                child: None,
                writer: BufWriter::new(Box::new(io::stdout())),
            })
        }
    }

    fn finish(mut self) -> io::Result<()> {
        let flush_result = self.writer.flush();
        drop(self.writer);
        let wait_result = if let Some(mut child) = self.child.take() {
            child.wait().and_then(|status| {
                if !status.success() && status.code() != Some(1) {
                    Err(io::Error::other(format!("less exited with {status}")))
                } else {
                    Ok(())
                }
            })
        } else {
            Ok(())
        };
        flush_result.and(wait_result)
    }
}

fn color_enabled(mode: Mode, terminal: bool) -> bool {
    match mode {
        Mode::Always => true,
        Mode::Never => false,
        Mode::Auto => {
            terminal
                && env::var_os("NO_COLOR").is_none()
                && env::var("TERM").ok().as_deref() != Some("dumb")
        }
    }
}

fn write_colored<W: Write>(out: &mut W, text: &str, color: bool) -> io::Result<()> {
    if !color {
        return out.write_all(text.as_bytes());
    }
    let bytes = text.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'"' {
            let start = i;
            i += 1;
            while i < bytes.len() {
                if bytes[i] == b'\\' {
                    i = (i + 2).min(bytes.len());
                } else if bytes[i] == b'"' {
                    i += 1;
                    break;
                } else {
                    i += 1;
                }
            }
            let mut next = i;
            while next < bytes.len() && bytes[next].is_ascii_whitespace() {
                next += 1;
            }
            let is_key = bytes.get(next) == Some(&b':');
            out.write_all(if is_key { b"\x1b[36m" } else { b"\x1b[32m" })?;
            out.write_all(&bytes[start..i])?;
            out.write_all(b"\x1b[0m")?;
        } else if bytes[i] == b'-' || bytes[i].is_ascii_digit() {
            let start = i;
            i += 1;
            while i < bytes.len()
                && (bytes[i].is_ascii_digit()
                    || matches!(bytes[i], b'.' | b'e' | b'E' | b'+' | b'-'))
            {
                i += 1;
            }
            out.write_all(b"\x1b[33m")?;
            out.write_all(&bytes[start..i])?;
            out.write_all(b"\x1b[0m")?;
        } else if bytes[i..].starts_with(b"true")
            || bytes[i..].starts_with(b"false")
            || bytes[i..].starts_with(b"null")
        {
            let n = if bytes[i..].starts_with(b"false") {
                5
            } else {
                4
            };
            out.write_all(b"\x1b[35m")?;
            out.write_all(&bytes[i..i + n])?;
            out.write_all(b"\x1b[0m")?;
            i += n;
        } else {
            out.write_all(&bytes[i..i + 1])?;
            i += 1;
        }
    }
    Ok(())
}

enum RenderError {
    Io(io::Error),
    Message(String),
}

impl From<io::Error> for RenderError {
    fn from(error: io::Error) -> Self {
        Self::Io(error)
    }
}

fn render<R: Read, W: Write>(
    reader: R,
    out: &mut W,
    color: bool,
    source: &str,
) -> Result<(), RenderError> {
    let mut reader = BufReader::new(reader);
    let mut line = Vec::new();
    let (mut physical, mut record) = (0usize, 0usize);
    loop {
        line.clear();
        let count = reader
            .read_until(b'\n', &mut line)
            .map_err(|e| RenderError::Message(format!("{source}: read error: {e}")))?;
        if count == 0 {
            break;
        }
        physical += 1;
        if line.last() == Some(&b'\n') {
            line.pop();
        }
        if line.last() == Some(&b'\r') {
            line.pop();
        }
        if line.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        let value: serde_json::Value = serde_json::from_slice(&line).map_err(|e| {
            RenderError::Message(format!(
                "{source}: invalid JSON at physical line {physical}: {e}"
            ))
        })?;
        let pretty = serde_json::to_string_pretty(&value)
            .map_err(|e| RenderError::Message(format!("format error: {e}")))?;
        record += 1;
        writeln!(out, "record {record} | source line {physical}")?;
        write_colored(out, &pretty, color)?;
        out.write_all(b"\n\n")?;
        out.flush()?;
    }
    Ok(())
}

fn run(options: Options) -> Result<(), String> {
    let input_tty = io::stdin().is_terminal();
    let output_tty = io::stdout().is_terminal();
    if options.interactive {
        if options.input == "-" {
            return Err("--interactive requires a named input file".into());
        }
        if !input_tty || !output_tty {
            return Err("--interactive requires stdin and stdout to be terminals".into());
        }
    }
    let use_interactive = options.interactive
        || (!options.plain
            && options.paging == Mode::Auto
            && options.input != "-"
            && input_tty
            && output_tty);
    if use_interactive {
        let color = color_enabled(options.color, output_tty);
        return tui::run(std::path::Path::new(&options.input), color);
    }
    // Open named input before starting less so a bad path never opens an empty pager.
    let input: Box<dyn Read> = if options.input == "-" {
        Box::new(io::stdin())
    } else {
        Box::new(File::open(&options.input).map_err(|e| format!("{}: {e}", options.input))?)
    };
    let terminal = output_tty;
    let paging = match options.paging {
        Mode::Always => true,
        Mode::Never => false,
        Mode::Auto => {
            terminal
                && Command::new("less")
                    .arg("--version")
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .status()
                    .is_ok_and(|s| s.success())
        }
    };
    let color = color_enabled(options.color, terminal);
    let mut output = Output::new(paging).map_err(|e| format!("could not start less: {e}"))?;
    let rendered = render(input, &mut output.writer, color, &options.input);
    let finished = output.finish();
    match rendered {
        Ok(()) => match finished {
            Err(error) if error.kind() == io::ErrorKind::BrokenPipe => Ok(()),
            Err(error) => Err(error.to_string()),
            Ok(()) => Ok(()),
        },
        Err(RenderError::Io(error)) if error.kind() == io::ErrorKind::BrokenPipe => Ok(()),
        Err(RenderError::Io(error)) => Err(format!("{}: write error: {error}", options.input)),
        Err(RenderError::Message(error)) => {
            let _ = finished;
            Err(error)
        }
    }
}

fn main() {
    match parse_args() {
        Ok(None) => println!("{}", usage()),
        Err(error) => {
            eprintln!("jcat: {error}\n\n{}", usage());
            std::process::exit(2);
        }
        Ok(Some(options)) => {
            if let Err(error) = run(options) {
                eprintln!("jcat: {error}");
                std::process::exit(1);
            }
        }
    }
}
