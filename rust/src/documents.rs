use ansi_to_tui::IntoText;
use quick_xml::{Reader, events::Event};
use ratatui::text::Text;
use regex::Regex;
use std::sync::LazyLock;
use termimad::MadSkin;

static MARKDOWN_SYNTAX: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?m)^ {0,3}(#{1,6}[ \t]|`{3,}|~{3,}|>[ \t]|[-+*][ \t]|[0-9]+[.)][ \t])|\*\*[^*\n]+\*\*|__[^_\n]+__|\*[^*\n]+\*|`[^`\n]+`|\[[^\]\n]+\]\(")
        .expect("valid Markdown detection pattern")
});

pub(crate) fn render_field(text: &str) -> Option<Text<'static>> {
    let rendered = if text.trim_start().starts_with('<') {
        render_xml(text).ok()?
    } else if MARKDOWN_SYNTAX.is_match(text) {
        let normalized = super::tui::safe_text(&text.replace("\r\n", "\n"));
        MadSkin::default().text(&normalized, None).to_string()
    } else {
        return None;
    };
    let output = rendered.into_text().ok()?;
    (!output.lines.is_empty()).then_some(output)
}

#[derive(Default)]
struct Container {
    parent: usize,
    preserve: bool,
    has_text: bool,
    has_markup: bool,
}

struct Piece<'a> {
    text: &'a str,
    parent: usize,
    depth: usize,
    markup: bool,
}

// Reindent element-only containers, keeping mixed content and xml:space intact.
fn render_xml(text: &str) -> Result<String, String> {
    if text.chars().any(|c| !xml_char(c)) {
        return Err("invalid XML character".into());
    }
    let mut containers = vec![Container::default()];
    let mut stack = vec![0];
    let mut pieces = Vec::new();
    let mut reader = Reader::from_str(text);
    reader.config_mut().check_comments = true;
    let mut roots = 0;
    loop {
        let start = reader.buffer_position() as usize;
        let event = reader
            .read_event()
            .map_err(|err| format!("at byte {}: {err}", reader.error_position() + 1))?;
        let depth = stack.len() - 1;
        let parent = stack[depth];
        let raw = &text[start..reader.buffer_position() as usize];
        let mut piece = Piece {
            text: raw,
            parent,
            depth,
            markup: true,
        };
        match event {
            Event::Eof => break,
            Event::Start(ref tag) | Event::Empty(ref tag) => {
                if tag.name().as_ref().is_empty() {
                    return Err("empty element name".into());
                }
                if depth == 0 {
                    roots += 1;
                }
                containers[parent].has_markup = true;
                let mut preserve = containers[parent].preserve;
                for attr in tag.attributes() {
                    let attr = attr.map_err(|err| err.to_string())?;
                    let value = attr
                        .decode_and_unescape_value(reader.decoder())
                        .map_err(|err| err.to_string())?;
                    if attr.key.as_ref() == b"xml:space" {
                        preserve = value != "default";
                    }
                }
                if matches!(event, Event::Start(_)) {
                    stack.push(containers.len());
                    containers.push(Container {
                        parent,
                        preserve,
                        ..Container::default()
                    });
                }
            }
            Event::End(_) => {
                stack.pop();
                piece.depth -= 1;
            }
            Event::Text(_) | Event::CData(_) | Event::GeneralRef(_) => {
                piece.markup = false;
                if !raw.trim_matches([' ', '\t', '\r', '\n']).is_empty() {
                    if depth == 0 {
                        return Err("text outside the root element".into());
                    }
                    containers[parent].has_text = true;
                }
                if let Event::GeneralRef(reference) = event {
                    let predefined =
                        matches!(&*reference, b"amp" | b"lt" | b"gt" | b"apos" | b"quot");
                    let character = reference
                        .resolve_char_ref()
                        .map_err(|err| err.to_string())?;
                    if !predefined && !character.is_some_and(xml_char) {
                        return Err(format!("unknown entity {raw}"));
                    }
                }
            }
            _ => containers[parent].has_markup = true,
        }
        pieces.push(piece);
    }
    if stack.len() != 1 {
        return Err("unclosed element".into());
    }
    if roots == 0 {
        return Err("expected an element".into());
    }
    for i in 1..containers.len() {
        containers[i].has_text |= containers[containers[i].parent].has_text;
    }

    let mut output = String::new();
    for piece in pieces {
        let parent = &containers[piece.parent];
        if !parent.preserve && !parent.has_text && parent.has_markup {
            if !piece.markup {
                continue;
            }
            if !output.is_empty() {
                output.push('\n');
            }
            output.push_str(&"  ".repeat(piece.depth));
        }
        let text = super::tui::safe_text(piece.text);
        if piece.markup {
            output.push_str("\x1b[36m");
            output.push_str(&text.replace('\n', "\x1b[0m\n\x1b[36m"));
            output.push_str("\x1b[0m");
        } else {
            output.push_str(&text);
        }
    }
    Ok(output)
}

fn xml_char(c: char) -> bool {
    matches!(c, '\t' | '\n' | '\r' | '\u{20}'..='\u{d7ff}' | '\u{e000}'..='\u{fffd}' | '\u{10000}'..='\u{10ffff}')
}
