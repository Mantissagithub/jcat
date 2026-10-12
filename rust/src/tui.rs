use std::collections::HashMap;
use std::fs::File;
use std::io::{self, BufRead, BufReader, Read, Seek, SeekFrom};
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyModifiers};
use ratatui::layout::{Constraint, Direction, Layout, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span, Text};
use ratatui::widgets::{
    Block, BorderType, Borders, Clear, List, ListItem, ListState, Paragraph, Wrap,
};
use ratatui::{Terminal, backend::CrosstermBackend};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};

const PREVIEW_CHARS: usize = 72;
const PALETTE_CYAN: Color = Color::Rgb(85, 221, 240);
const PALETTE_TEXT: Color = Color::Rgb(168, 201, 204);
const PALETTE_AMBER: Color = Color::Rgb(244, 184, 96);
const PALETTE_LAVENDER: Color = Color::Rgb(199, 146, 234);
const PALETTE_SLATE: Color = Color::Rgb(112, 135, 145);
const PALETTE_BORDER: Color = Color::Rgb(66, 91, 98);
const PALETTE_SELECTION: Color = Color::Rgb(37, 66, 72);
const PALETTE_SEARCH: Color = Color::Rgb(83, 67, 42);

#[derive(Clone, Copy, Debug, PartialEq)]
enum ValueKind {
    Section,
    Key,
    String,
    Number,
    Boolean,
    Null,
    Raw,
}

#[derive(Clone, Debug)]
struct DisplayLine {
    depth: usize,
    label: String,
    label_width: usize,
    value: String,
    kind: ValueKind,
    search_text: Arc<str>,
    source_id: usize,
    start: usize,
    end: usize,
}

#[derive(Clone, Debug)]
struct Entry {
    offset: u64,
    len: u64,
    source_line: usize,
    preview: String,
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum Focus {
    List,
    Inspector,
}
#[derive(Clone, Copy, Debug, PartialEq)]
enum SearchTarget {
    List,
    Inspector,
}

struct State {
    entries: Vec<Entry>,
    visible: Vec<usize>,
    selected: usize,
    focus: Focus,
    left_query: Option<String>,
    right_query: Option<String>,
    prompt: Option<(SearchTarget, String)>,
    help: bool,
    help_scroll: u16,
    inspector_scroll: usize,
    inspector_horizontal: usize,
    inspector_rows: usize,
    list_rows: usize,
    inspector_width: usize,
    raw_mode: bool,
    cached_raw: bool,
    cached_width: usize,
    cached_entry: Option<usize>,
    cached_lines: Vec<DisplayLine>,
    cached_error: Option<String>,
    search_lines: Vec<(usize, usize)>,
    search_cursor: usize,
    source: String,
    color: bool,
    preview_cache: HashMap<usize, String>,
}

fn index_file(path: &Path) -> io::Result<Vec<Entry>> {
    let mut reader = BufReader::new(File::open(path)?);
    let mut entries = Vec::new();
    let mut offset = 0u64;
    let mut source_line = 0usize;
    let mut bytes = Vec::new();
    loop {
        bytes.clear();
        let len = reader.read_until(b'\n', &mut bytes)?;
        if len == 0 {
            break;
        }
        source_line += 1;
        let start = offset;
        offset += len as u64;
        let mut content_len = bytes.len();
        while content_len > 0 && matches!(bytes[content_len - 1], b'\n' | b'\r') {
            content_len -= 1;
        }
        let content = &bytes[..content_len];
        if content.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        let preview_text = String::from_utf8_lossy(content);
        let mut preview = preview_text
            .chars()
            .take(PREVIEW_CHARS + 1)
            .collect::<String>();
        if preview.chars().count() > PREVIEW_CHARS {
            preview.pop();
            preview.push('…');
        }
        entries.push(Entry {
            offset: start,
            len: len as u64,
            source_line,
            preview,
        });
    }
    Ok(entries)
}

fn load_record(
    file: &mut File,
    entry: &Entry,
    raw_mode: bool,
    width: usize,
) -> io::Result<(Vec<DisplayLine>, Option<String>)> {
    file.seek(SeekFrom::Start(entry.offset))?;
    let mut raw = vec![0; entry.len as usize];
    file.read_exact(&mut raw)?;
    while matches!(raw.last(), Some(b'\n' | b'\r')) {
        raw.pop();
    }
    match serde_json::from_slice::<serde_json::Value>(&raw) {
        Ok(value) if raw_mode => Ok((
            serde_json::to_string_pretty(&value)
                .unwrap_or_default()
                .lines()
                .enumerate()
                .map(|(index, line)| DisplayLine {
                    depth: 0,
                    label: String::new(),
                    label_width: 0,
                    value: line.to_owned(),
                    kind: ValueKind::Raw,
                    search_text: Arc::from(line.to_owned()),
                    source_id: index + 1,
                    start: 0,
                    end: line.len(),
                })
                .collect(),
            None,
        )),
        Ok(value) => Ok((display_fields(&value, width), None)),
        Err(error) => Ok((
            Vec::new(),
            Some(format!(
                "Invalid JSON at physical line {}: {error}",
                entry.source_line
            )),
        )),
    }
}

fn display_fields(value: &serde_json::Value, width: usize) -> Vec<DisplayLine> {
    let mut lines = Vec::new();
    let mut sequence = 0usize;
    match value {
        serde_json::Value::Object(object) if object.is_empty() => lines.push(DisplayLine {
            depth: 0,
            label: String::new(),
            label_width: 0,
            value: "Empty object".into(),
            kind: ValueKind::Section,
            search_text: Arc::from("Empty object"),
            source_id: 0,
            start: 0,
            end: 0,
        }),
        serde_json::Value::Object(object) => {
            append_object(&mut lines, object, 0, width.max(18), &mut sequence)
        }
        _ => append_value(
            &mut lines,
            "Value",
            value,
            0,
            8,
            width.max(18),
            &mut sequence,
        ),
    }
    lines
}

fn append_object(
    lines: &mut Vec<DisplayLine>,
    object: &serde_json::Map<String, serde_json::Value>,
    depth: usize,
    width: usize,
    sequence: &mut usize,
) {
    let label_width = object
        .keys()
        .map(|key| UnicodeWidthStr::width(safe_text(key).as_str()))
        .max()
        .unwrap_or(0)
        .min((width / 2).max(12));
    for (key, value) in object {
        let label = safe_text(key);
        if UnicodeWidthStr::width(label.as_str()) > label_width {
            let shared: Arc<str> = Arc::from(label.as_str());
            for (segment, start, end) in wrap_segments(&label, label_width) {
                *sequence += 1;
                lines.push(DisplayLine {
                    depth,
                    label_width: UnicodeWidthStr::width(segment.as_str()),
                    label: segment,
                    value: String::new(),
                    kind: ValueKind::Key,
                    search_text: Arc::clone(&shared),
                    source_id: *sequence,
                    start,
                    end,
                });
            }
            append_value(lines, "", value, depth, 0, width, sequence);
        } else {
            append_value(lines, &label, value, depth, label_width, width, sequence);
        }
    }
}

fn append_value(
    lines: &mut Vec<DisplayLine>,
    label: &str,
    value: &serde_json::Value,
    depth: usize,
    label_width: usize,
    width: usize,
    sequence: &mut usize,
) {
    *sequence += 1;
    let source_id = *sequence;
    let value_width = width.saturating_sub(depth * 2 + label_width + 4).max(12);
    let mut push =
        |value: String, kind: ValueKind, search_text: String, start: usize, end: usize| {
            lines.push(DisplayLine {
                depth,
                label: label.to_owned(),
                label_width,
                value,
                kind,
                search_text: Arc::from(search_text),
                source_id,
                start,
                end,
            });
        };
    match value {
        serde_json::Value::Object(object) => {
            if object.is_empty() {
                push(
                    "Empty object".into(),
                    ValueKind::Section,
                    format!("{label} Empty object"),
                    0,
                    0,
                );
            } else {
                let summary = format!("Object · {} fields", object.len());
                push(
                    summary.clone(),
                    ValueKind::Section,
                    format!("{label} {summary}"),
                    0,
                    0,
                );
                append_object(lines, object, depth + 1, width, sequence);
            }
        }
        serde_json::Value::Array(items) => {
            if items.is_empty() {
                push(
                    "Empty array".into(),
                    ValueKind::Section,
                    format!("{label} Empty array"),
                    0,
                    0,
                );
            } else {
                let summary = format!("Array · {} items", items.len());
                push(
                    summary.clone(),
                    ValueKind::Section,
                    format!("{label} {summary}"),
                    0,
                    0,
                );
                let item_width = items.len().to_string().len().max(2);
                for (index, item) in items.iter().enumerate() {
                    append_value(
                        lines,
                        &format!("{:0item_width$}", index + 1),
                        item,
                        depth + 1,
                        item_width,
                        width,
                        sequence,
                    );
                }
            }
        }
        serde_json::Value::String(text) => {
            if text.is_empty() {
                push(
                    "Empty string".into(),
                    ValueKind::String,
                    format!("{label} Empty string"),
                    0,
                    0,
                );
            } else {
                let safe = safe_text(text);
                let shared: Arc<str> = Arc::from(safe.as_str());
                let mut first = true;
                for (segment, start, end) in wrap_segments(&safe, value_width) {
                    lines.push(DisplayLine {
                        depth,
                        label: if first {
                            label.to_owned()
                        } else {
                            String::new()
                        },
                        label_width,
                        value: segment,
                        kind: ValueKind::String,
                        search_text: Arc::clone(&shared),
                        source_id,
                        start,
                        end,
                    });
                    first = false;
                }
            }
        }
        serde_json::Value::Number(number) => {
            let number = number.to_string();
            let shared: Arc<str> = Arc::from(number.as_str());
            let mut first = true;
            for (segment, start, end) in wrap_segments(&number, value_width) {
                lines.push(DisplayLine {
                    depth,
                    label: if first {
                        label.to_owned()
                    } else {
                        String::new()
                    },
                    label_width,
                    value: segment,
                    kind: ValueKind::Number,
                    search_text: Arc::clone(&shared),
                    source_id,
                    start,
                    end,
                });
                first = false;
            }
        }
        serde_json::Value::Bool(boolean) => push(
            boolean.to_string(),
            ValueKind::Boolean,
            format!("{label} {boolean}"),
            0,
            0,
        ),
        serde_json::Value::Null => push(
            "Null".into(),
            ValueKind::Null,
            format!("{label} Null"),
            0,
            0,
        ),
    }
}

fn safe_text(text: &str) -> String {
    let mut result = String::with_capacity(text.len());
    for character in text.chars() {
        match character {
            '\n' => result.push('\n'),
            '\t' => result.push_str("\\t"),
            '\r' => result.push_str("\\r"),
            c if c.is_control() => result.push_str(&format!("\\u{:04x}", c as u32)),
            c => result.push(c),
        }
    }
    result
}

fn wrap_segments(text: &str, width: usize) -> Vec<(String, usize, usize)> {
    let mut result = Vec::new();
    let mut paragraph_start = 0;
    loop {
        let paragraph_end = text[paragraph_start..]
            .find('\n')
            .map_or(text.len(), |offset| paragraph_start + offset);
        if paragraph_start == paragraph_end {
            result.push((String::new(), paragraph_start, paragraph_end));
        } else {
            let mut start = paragraph_start;
            while start < paragraph_end {
                let mut cursor = start;
                let mut cells = 0usize;
                let mut last_break = None;
                while cursor < paragraph_end {
                    let character = text[cursor..paragraph_end].chars().next().unwrap();
                    let size = character.len_utf8();
                    let char_width = UnicodeWidthChar::width(character).unwrap_or(0);
                    if cells + char_width > width && cursor > start {
                        break;
                    }
                    cursor += size;
                    cells += char_width;
                    if character.is_whitespace() {
                        last_break = Some(cursor);
                    }
                }
                let end = if cursor < paragraph_end {
                    last_break.filter(|index| *index > start).unwrap_or(cursor)
                } else {
                    cursor
                };
                let end = end.max(start + text[start..].chars().next().unwrap().len_utf8());
                result.push((text[start..end].to_owned(), start, end));
                start = end;
            }
        }
        if paragraph_end == text.len() {
            break;
        }
        paragraph_start = paragraph_end + 1;
    }
    result
}

impl State {
    fn new(path: &Path, entries: Vec<Entry>, color: bool) -> Self {
        let visible = (0..entries.len()).collect();
        Self {
            entries,
            visible,
            selected: 0,
            focus: Focus::List,
            left_query: None,
            right_query: None,
            prompt: None,
            help: false,
            help_scroll: 0,
            inspector_scroll: 0,
            inspector_horizontal: 0,
            inspector_rows: 1,
            list_rows: 1,
            inspector_width: 48,
            raw_mode: false,
            cached_raw: false,
            cached_width: 0,
            cached_entry: None,
            cached_lines: Vec::new(),
            cached_error: None,
            search_lines: Vec::new(),
            search_cursor: 0,
            source: path
                .file_name()
                .unwrap_or(path.as_os_str())
                .to_string_lossy()
                .into_owned(),
            color,
            preview_cache: HashMap::new(),
        }
    }
    fn cache_previews(&mut self, file: &mut File) -> io::Result<()> {
        let first = self
            .selected
            .saturating_sub(self.list_rows.saturating_sub(1));
        let end = (first + self.list_rows).min(self.visible.len());
        for entry_index in self.visible.get(first..end).unwrap_or(&[]).iter().copied() {
            if self.preview_cache.contains_key(&entry_index) {
                continue;
            }
            if self.preview_cache.len() >= 128 {
                self.preview_cache.clear();
            }
            let entry = &self.entries[entry_index];
            file.seek(SeekFrom::Start(entry.offset))?;
            let mut raw = vec![0; entry.len as usize];
            file.read_exact(&mut raw)?;
            let summary = serde_json::from_slice::<serde_json::Value>(&raw)
                .ok()
                .and_then(record_summary)
                .unwrap_or_else(|| entry.preview.clone());
            self.preview_cache.insert(entry_index, summary);
        }
        Ok(())
    }
    fn selected_entry(&self) -> Option<usize> {
        self.visible.get(self.selected).copied()
    }
    fn clamp_inspector(&mut self) {
        self.inspector_scroll = self
            .inspector_scroll
            .min(self.cached_lines.len().saturating_sub(self.inspector_rows));
    }
    fn filter(&mut self, file: &mut File) -> io::Result<()> {
        let query = self.left_query.as_ref().map(|q| q.to_lowercase());
        self.visible.clear();
        for (i, entry) in self.entries.iter().enumerate() {
            let found = match &query {
                Some(q) => record_matches(file, entry, q)?,
                None => true,
            };
            if found {
                self.visible.push(i);
            }
        }
        self.selected = 0;
        self.inspector_scroll = 0;
        self.inspector_horizontal = 0;
        self.cached_entry = None;
        self.search_lines.clear();
        self.clamp_inspector();
        Ok(())
    }
    fn update_selected(&mut self, file: &mut File) {
        let Some(index) = self.selected_entry() else {
            self.cached_entry = None;
            self.cached_lines.clear();
            self.cached_error = None;
            return;
        };
        if self.cached_entry == Some(index)
            && self.cached_width == self.inspector_width
            && self.cached_raw == self.raw_mode
        {
            return;
        }
        self.cached_lines.clear();
        match load_record(
            file,
            &self.entries[index],
            self.raw_mode,
            self.inspector_width,
        ) {
            Ok((lines, error)) => {
                self.cached_lines = lines;
                self.cached_error = error;
            }
            Err(e) => {
                self.cached_lines.clear();
                self.cached_error = Some(format!("Read error: {e}"));
            }
        }
        self.cached_entry = Some(index);
        self.cached_width = self.inspector_width;
        self.cached_raw = self.raw_mode;
        self.rebuild_search_lines();
    }
    fn rebuild_search_lines(&mut self) {
        self.search_lines.clear();
        self.search_cursor = 0;
        if let Some(query) = self.right_query.as_ref().filter(|q| !q.is_empty()) {
            let q = query.to_lowercase();
            let mut seen = HashMap::new();
            for (i, line) in self.cached_lines.iter().enumerate() {
                if seen.contains_key(&line.source_id) {
                    continue;
                }
                seen.insert(line.source_id, ());
                if line.kind == ValueKind::Key {
                    if let Some(byte) = folded_index(&line.search_text, &q)
                        && line.start <= byte
                        && byte < line.end
                    {
                        let relative = byte - line.start;
                        let column =
                            line.depth * 2 + UnicodeWidthStr::width(&line.label[..relative]);
                        self.search_lines.push((i, column));
                    }
                    continue;
                }
                if !line.label.is_empty() && line.label.to_lowercase().contains(&q) {
                    self.search_lines.push((i, 0));
                    continue;
                }
                let Some(byte) = folded_index(&line.search_text, &q) else {
                    continue;
                };
                let row = self.cached_lines[i..]
                    .iter()
                    .enumerate()
                    .take_while(|(_, candidate)| candidate.source_id == line.source_id)
                    .find_map(|(offset, candidate)| {
                        (candidate.start <= byte && byte < candidate.end).then_some(i + offset)
                    })
                    .unwrap_or(i);
                let segment = &self.cached_lines[row];
                let start = segment.start;
                let column = segment.depth * 2
                    + segment.label_width
                    + 2
                    + UnicodeWidthStr::width(&line.search_text[start..byte]);
                self.search_lines.push((row, column));
            }
        }
        if let Some((line, column)) = self.search_lines.first() {
            self.inspector_scroll = *line;
            self.inspector_horizontal = *column;
        } else if self.right_query.is_some() {
            self.inspector_scroll = 0;
            self.inspector_horizontal = 0;
        }
    }
    fn move_selection(&mut self, delta: isize) {
        if !self.visible.is_empty() {
            self.selected =
                (self.selected as isize + delta).clamp(0, self.visible.len() as isize - 1) as usize;
            self.inspector_scroll = 0;
            self.inspector_horizontal = 0;
            self.cached_entry = None;
        }
    }
    fn move_to(&mut self, index: usize) {
        if !self.visible.is_empty() {
            self.selected = index.min(self.visible.len() - 1);
            self.inspector_scroll = 0;
            self.inspector_horizontal = 0;
            self.cached_entry = None;
        }
    }
    fn jump_match(&mut self, forward: bool) {
        if self.search_lines.is_empty() {
            return;
        }
        self.search_cursor = if forward {
            (self.search_cursor + 1) % self.search_lines.len()
        } else {
            (self.search_cursor + self.search_lines.len() - 1) % self.search_lines.len()
        };
        (self.inspector_scroll, self.inspector_horizontal) = self.search_lines[self.search_cursor];
    }
}

// Return a byte offset in the source string, even when Unicode lowercase
// expands a character (for example, U+0130 LATIN CAPITAL I WITH DOT ABOVE).
fn folded_index(text: &str, query: &str) -> Option<usize> {
    let mut folded = String::with_capacity(text.len());
    let mut source_offsets = Vec::with_capacity(text.len());
    for (offset, character) in text.char_indices() {
        let lower = character.to_lowercase().collect::<String>();
        folded.push_str(&lower);
        source_offsets.extend(std::iter::repeat_n(offset, lower.len()));
    }
    folded
        .find(query)
        .and_then(|index| source_offsets.get(index).copied())
}

fn record_summary(value: serde_json::Value) -> Option<String> {
    let object = value.as_object()?;
    let value_for = |keys: &[&str]| {
        keys.iter().find_map(|key| {
            object.get(*key).and_then(|v| match v {
                serde_json::Value::String(s) => Some(s.clone()),
                serde_json::Value::Number(n) => Some(n.to_string()),
                serde_json::Value::Bool(b) => Some(b.to_string()),
                _ => None,
            })
        })
    };
    let primary = value_for(&["event", "type", "role", "name", "method"])?;
    let mut parts = vec![primary];
    if let Some(detail) = value_for(&[
        "message",
        "content",
        "path",
        "service",
        "status",
        "duration_ms",
    ]) {
        parts.push(detail);
    }
    if let Some(level) = value_for(&["level", "severity"]) {
        parts.push(level);
    }
    let joined = parts.join(" · ");
    let mut summary = joined.chars().take(PREVIEW_CHARS).collect::<String>();
    if joined.chars().count() > PREVIEW_CHARS {
        summary.pop();
        summary.push('…');
    }
    Some(summary)
}

fn record_matches(file: &mut File, entry: &Entry, query: &str) -> io::Result<bool> {
    file.seek(SeekFrom::Start(entry.offset))?;
    let mut raw = vec![0; entry.len as usize];
    file.read_exact(&mut raw)?;
    Ok(String::from_utf8_lossy(&raw).to_lowercase().contains(query))
}

fn centered(area: Rect, width: u16, height: u16) -> Rect {
    Rect {
        x: area.x + area.width.saturating_sub(width) / 2,
        y: area.y + area.height.saturating_sub(height) / 2,
        width: width.min(area.width),
        height: height.min(area.height),
    }
}

fn slice_cells(text: &str, skip: usize, take: usize) -> String {
    let mut result = String::new();
    let mut cells = 0usize;
    for character in text.chars() {
        let width = UnicodeWidthChar::width(character).unwrap_or(0);
        if cells >= skip && cells < skip.saturating_add(take) {
            result.push(character);
        }
        cells += width;
        if cells >= skip.saturating_add(take) {
            break;
        }
    }
    result
}

fn json_line(line: &str, query: Option<&str>, color: bool) -> Line<'static> {
    let style = Style::default().fg(Color::Gray);
    let mut spans = Vec::new();
    let mut chars = line.char_indices().peekable();
    let q = query.map(|q| q.to_lowercase());
    while let Some((start, ch)) = chars.next() {
        let mut end = start + ch.len_utf8();
        if ch == '"' {
            while let Some((i, c)) = chars.next() {
                end = i + c.len_utf8();
                if c == '\\' {
                    if let Some((j, n)) = chars.next() {
                        end = j + n.len_utf8();
                    }
                } else if c == '"' {
                    break;
                }
            }
            let mut look = end;
            while line[look..].starts_with(char::is_whitespace) {
                let c = line[look..].chars().next().unwrap();
                look += c.len_utf8();
            }
            spans.push(Span::styled(
                line[start..end].to_owned(),
                Style::default().fg(if line[look..].starts_with(':') {
                    Color::Cyan
                } else {
                    Color::Green
                }),
            ));
        } else if ch.is_ascii_digit() || ch == '-' {
            while let Some((i, c)) = chars.peek().copied() {
                if c.is_ascii_digit() || matches!(c, '.' | 'e' | 'E' | '+' | '-') {
                    chars.next();
                    end = i + c.len_utf8();
                } else {
                    break;
                }
            }
            spans.push(Span::styled(
                line[start..end].to_owned(),
                Style::default().fg(Color::Yellow),
            ));
        } else if ch.is_ascii_alphabetic() {
            while let Some((i, c)) = chars.peek().copied() {
                if c.is_ascii_alphabetic() {
                    chars.next();
                    end = i + c.len_utf8();
                } else {
                    break;
                }
            }
            let word = &line[start..end];
            spans.push(Span::styled(
                word.to_owned(),
                Style::default().fg(if matches!(word, "true" | "false" | "null") {
                    Color::Magenta
                } else {
                    Color::Gray
                }),
            ));
        } else {
            spans.push(Span::styled(ch.to_string(), style));
        }
    }
    let line_matches = q
        .as_ref()
        .is_some_and(|q| !q.is_empty() && line.to_lowercase().contains(q));
    for span in &mut spans {
        if line_matches {
            span.style = if color {
                span.style.bg(Color::DarkGray).add_modifier(Modifier::BOLD)
            } else {
                Style::default().add_modifier(Modifier::REVERSED)
            };
        } else if !color {
            span.style = Style::default();
        }
    }
    Line::from(spans)
}

fn draw(frame: &mut ratatui::Frame, state: &mut State) {
    let area = frame.area();
    if area.width < 50 || area.height < 10 {
        frame.render_widget(
            Paragraph::new("Resize terminal to at least 50 columns × 10 rows (q quits)."),
            area,
        );
        return;
    }
    let chunks = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(1),
            Constraint::Min(3),
            Constraint::Length(1),
        ])
        .split(area);
    let mode = if state.raw_mode {
        " RAW JSON "
    } else {
        " FIELDS "
    };
    frame.render_widget(
        Paragraph::new(Line::from(vec![
            Span::styled(
                format!(" jcat  {}  ", state.source),
                if state.color {
                    Style::default()
                        .fg(PALETTE_CYAN)
                        .add_modifier(Modifier::BOLD)
                } else {
                    Style::default().add_modifier(Modifier::BOLD)
                },
            ),
            Span::styled(
                mode,
                if state.color {
                    Style::default()
                        .fg(Color::Rgb(18, 30, 34))
                        .bg(PALETTE_CYAN)
                        .add_modifier(Modifier::BOLD)
                } else {
                    Style::default().add_modifier(Modifier::REVERSED | Modifier::BOLD)
                },
            ),
        ])),
        chunks[0],
    );
    let panes = Layout::default()
        .direction(Direction::Horizontal)
        .constraints([Constraint::Percentage(35), Constraint::Percentage(65)])
        .split(chunks[1]);
    state.inspector_width = panes[1].width.saturating_sub(8) as usize;
    let list_title = if let Some(q) = &state.left_query {
        format!(
            "Records {}/{} · /{}",
            state.visible.len(),
            state.entries.len(),
            q
        )
    } else {
        format!("Records {}/{}", state.visible.len(), state.entries.len())
    };
    let list_style = if state.color {
        if state.focus == Focus::List {
            Style::default().fg(PALETTE_CYAN)
        } else {
            Style::default().fg(PALETTE_BORDER)
        }
    } else {
        Style::default()
    };
    let list_rows = panes[0].height.saturating_sub(2) as usize;
    state.list_rows = list_rows.max(1);
    state.inspector_rows = panes[1].height.saturating_sub(2) as usize;
    state.clamp_inspector();
    let list_first = state.selected.saturating_sub(list_rows.saturating_sub(1));
    let list_end = (list_first + list_rows).min(state.visible.len());
    let preview_width = panes[0].width.saturating_sub(6) as usize;
    let visible_indices = state
        .visible
        .get(list_first..list_end)
        .unwrap_or(&[])
        .to_vec();
    let items = visible_indices
        .iter()
        .map(|i| {
            let e = &state.entries[*i];
            let preview = state
                .preview_cache
                .get(i)
                .unwrap_or(&e.preview)
                .chars()
                .take(preview_width)
                .collect::<String>();
            ListItem::new(Line::from(vec![
                Span::styled(
                    format!("#{} ", i + 1),
                    if state.color {
                        Style::default().fg(PALETTE_SLATE)
                    } else {
                        Style::default()
                    },
                ),
                Span::raw(preview),
            ]))
        })
        .collect::<Vec<_>>();
    let list = List::new(items)
        .block(
            Block::default()
                .title(list_title)
                .borders(Borders::ALL)
                .border_type(BorderType::Rounded)
                .border_style(list_style),
        )
        .highlight_style(if state.color {
            Style::default()
                .bg(PALETTE_SELECTION)
                .fg(PALETTE_TEXT)
                .add_modifier(Modifier::BOLD)
        } else {
            Style::default().add_modifier(Modifier::REVERSED | Modifier::BOLD)
        })
        .highlight_symbol("› ");
    let mut list_state = ListState::default();
    if !state.visible.is_empty() {
        list_state.select(Some(state.selected - list_first));
    }
    frame.render_stateful_widget(list, panes[0], &mut list_state);
    let selected = state.selected_entry().map(|i| &state.entries[i]);
    let mode = if state.raw_mode { "Raw JSON" } else { "Fields" };
    let right_title = selected
        .map(|e| {
            let suffix = if state.right_query.is_some() && state.search_lines.is_empty() {
                " · no matches"
            } else if state.right_query.is_some() {
                " · matches"
            } else {
                ""
            };
            format!(
                "{} · record {} · source line {}{}",
                mode,
                state.selected_entry().unwrap() + 1,
                e.source_line,
                suffix
            )
        })
        .unwrap_or_else(|| format!("{mode} · no records"));
    let border = if state.color {
        if state.focus == Focus::Inspector {
            Style::default().fg(PALETTE_CYAN)
        } else {
            Style::default().fg(PALETTE_BORDER)
        }
    } else {
        Style::default()
    };
    let content = if let Some(error) = &state.cached_error {
        Text::from(error.clone())
    } else if state.cached_lines.is_empty() {
        Text::from(if state.entries.is_empty() {
            "Empty file"
        } else if state.visible.is_empty() {
            "No records match this filter"
        } else {
            ""
        })
    } else {
        Text::from(
            state
                .cached_lines
                .iter()
                .enumerate()
                .skip(state.inspector_scroll)
                .take(panes[1].height.saturating_sub(2) as usize)
                .map(|(i, item)| {
                    let mut spans = Vec::new();
                    if item.kind == ValueKind::Raw {
                        spans.push(Span::styled(
                            format!("{:>4} │ ", i + 1),
                            if state.color {
                                Style::default().fg(PALETTE_SLATE)
                            } else {
                                Style::default()
                            },
                        ));
                        let visible = slice_cells(
                            &item.value,
                            state.inspector_horizontal,
                            panes[1].width.saturating_sub(8) as usize,
                        );
                        spans.extend(
                            json_line(&visible, state.right_query.as_deref(), state.color).spans,
                        );
                    } else {
                        let padding = item
                            .label_width
                            .saturating_sub(UnicodeWidthStr::width(item.label.as_str()));
                        let plain = format!(
                            "{}{}{}  {}",
                            " ".repeat(item.depth * 2),
                            item.label,
                            " ".repeat(padding),
                            item.value
                        );
                        if state.inspector_horizontal > 0 {
                            spans.push(Span::raw(slice_cells(
                                &plain,
                                state.inspector_horizontal,
                                panes[1].width.saturating_sub(8) as usize,
                            )));
                        } else {
                            spans.push(Span::raw(" ".repeat(item.depth * 2)));
                            if !item.label.is_empty() {
                                spans.push(Span::styled(
                                    item.label.clone(),
                                    if state.color {
                                        Style::default()
                                            .fg(PALETTE_CYAN)
                                            .add_modifier(Modifier::BOLD)
                                    } else {
                                        Style::default().add_modifier(Modifier::BOLD)
                                    },
                                ));
                            }
                            if item.kind != ValueKind::Key {
                                spans.push(Span::raw(" ".repeat(padding + 2)));
                                let value_style = if state.color {
                                    match item.kind {
                                        ValueKind::Key => Style::default(),
                                        ValueKind::Section => Style::default()
                                            .fg(PALETTE_AMBER)
                                            .add_modifier(Modifier::BOLD),
                                        ValueKind::String => Style::default().fg(PALETTE_TEXT),
                                        ValueKind::Number => Style::default().fg(PALETTE_AMBER),
                                        ValueKind::Boolean => Style::default()
                                            .fg(PALETTE_LAVENDER)
                                            .add_modifier(Modifier::BOLD),
                                        ValueKind::Null => Style::default()
                                            .fg(PALETTE_SLATE)
                                            .add_modifier(Modifier::ITALIC),
                                        ValueKind::Raw => Style::default(),
                                    }
                                } else if item.kind == ValueKind::Section {
                                    Style::default().add_modifier(Modifier::BOLD)
                                } else {
                                    Style::default()
                                };
                                spans.push(Span::styled(item.value.clone(), value_style));
                            }
                        }
                    }
                    if state
                        .search_lines
                        .iter()
                        .any(|(line_index, _)| *line_index == i)
                    {
                        for span in &mut spans {
                            span.style = if state.color {
                                span.style.bg(PALETTE_SEARCH).add_modifier(Modifier::BOLD)
                            } else {
                                Style::default().add_modifier(Modifier::REVERSED)
                            };
                        }
                    }
                    Line::from(spans)
                })
                .collect::<Vec<_>>(),
        )
    };
    frame.render_widget(
        Paragraph::new(content).block(
            Block::default()
                .title(right_title)
                .borders(Borders::ALL)
                .border_type(BorderType::Rounded)
                .border_style(border),
        ),
        panes[1],
    );
    let footer = if let Some((target, prompt)) = &state.prompt {
        let scope = match target {
            SearchTarget::List => "Records",
            SearchTarget::Inspector if state.raw_mode => "Raw JSON",
            SearchTarget::Inspector => "Fields",
        };
        format!(" /{}: {}", scope, prompt)
    } else if state.help {
        " Esc close help".to_string()
    } else {
        format!(
            " {}/{} rows · {} matches · r fields/raw │ Tab focus · j/k move · / search · ? help · q quit",
            state.visible.len(),
            state.entries.len(),
            state
                .right_query
                .as_ref()
                .map_or(0, |_| state.search_lines.len())
        )
    };
    frame.render_widget(
        Paragraph::new(footer).style(if state.color {
            Style::default().fg(PALETTE_CYAN)
        } else {
            Style::default()
        }),
        chunks[2],
    );
    if state.help {
        let rect = centered(
            area,
            area.width.saturating_sub(8).min(74),
            area.height.saturating_sub(4).min(18),
        );
        frame.render_widget(Clear, rect);
        let manual = "Keys\n  Tab             Switch panels\n  j/k, ↑/↓        Select record or scroll inspector\n  PageUp/PageDown, Home/End\n  /               Search focused panel\n  Enter           Apply search; Esc cancels prompt\n  Esc             Clear active search\n  n/N             Next/previous matching row\n  r               Toggle Fields / Raw JSON\n  h/l or ←/→    Scroll horizontally in either view\n  ?               Toggle this manual    q / Ctrl-C quit\n  Help: j/k or PgUp/PgDn scroll\n\nSearch manual\n  Records searches raw JSONL records. Fields searches displayed keys and values.\n  Raw JSON searches the selected pretty-printed record.\n  Search is case-insensitive literal text, not a regex or JSON field query.\n  Enter applies; Esc cancels or clears. No-results can be cleared with Esc.";
        frame.render_widget(
            Paragraph::new(manual)
                .scroll((state.help_scroll, 0))
                .block(
                    Block::default()
                        .title("Help / Search manual")
                        .borders(Borders::ALL)
                        .border_type(BorderType::Rounded)
                        .border_style(if state.color {
                            Style::default().fg(PALETTE_CYAN)
                        } else {
                            Style::default()
                        }),
                )
                .wrap(Wrap { trim: false }),
            rect,
        );
    }
}

fn handle(state: &mut State, file: &mut File, key: KeyEvent) -> Result<bool, String> {
    if key.code == KeyCode::Char('c') && key.modifiers.contains(KeyModifiers::CONTROL) {
        return Ok(true);
    }
    if state.help {
        match key.code {
            KeyCode::Char('q') => return Ok(true),
            KeyCode::Esc | KeyCode::Char('?') => state.help = false,
            KeyCode::Char('j') | KeyCode::Down => {
                state.help_scroll = state.help_scroll.saturating_add(1)
            }
            KeyCode::Char('k') | KeyCode::Up => {
                state.help_scroll = state.help_scroll.saturating_sub(1)
            }
            KeyCode::PageDown => state.help_scroll = state.help_scroll.saturating_add(8),
            KeyCode::PageUp => state.help_scroll = state.help_scroll.saturating_sub(8),
            _ => {}
        }
        return Ok(false);
    }
    if let Some((target, mut prompt)) = state.prompt.take() {
        match key.code {
            KeyCode::Esc => {}
            KeyCode::Enter => match target {
                SearchTarget::List => {
                    state.left_query = (!prompt.is_empty()).then_some(prompt);
                    state
                        .filter(file)
                        .map_err(|e| format!("search read error: {e}"))?;
                    state.update_selected(file);
                }
                SearchTarget::Inspector => {
                    state.right_query = (!prompt.is_empty()).then_some(prompt);
                    state.rebuild_search_lines();
                }
            },
            KeyCode::Backspace => {
                prompt.pop();
                state.prompt = Some((target, prompt));
            }
            KeyCode::Char(c) => {
                prompt.push(c);
                state.prompt = Some((target, prompt));
            }
            _ => state.prompt = Some((target, prompt)),
        }
        return Ok(false);
    }
    match key.code {
        KeyCode::Char('q') => return Ok(true),
        KeyCode::Char('?') => state.help = true,
        KeyCode::Tab => {
            state.focus = if state.focus == Focus::List {
                Focus::Inspector
            } else {
                Focus::List
            }
        }
        KeyCode::Char('/') => {
            state.prompt = Some((
                if state.focus == Focus::List {
                    SearchTarget::List
                } else {
                    SearchTarget::Inspector
                },
                String::new(),
            ))
        }
        KeyCode::Esc => {
            if state.focus == Focus::List {
                state.left_query = None;
                state
                    .filter(file)
                    .map_err(|e| format!("search read error: {e}"))?;
            } else {
                state.right_query = None;
                state.rebuild_search_lines();
            }
        }
        KeyCode::Char('j') | KeyCode::Down => {
            if state.focus == Focus::List {
                state.move_selection(1)
            } else {
                state.inspector_scroll = state.inspector_scroll.saturating_add(1);
                state.inspector_horizontal = 0;
            }
        }
        KeyCode::Char('h') | KeyCode::Left if state.focus == Focus::Inspector => {
            state.inspector_horizontal = state.inspector_horizontal.saturating_sub(20)
        }
        KeyCode::Char('l') | KeyCode::Right if state.focus == Focus::Inspector => {
            state.inspector_horizontal = state.inspector_horizontal.saturating_add(20)
        }
        KeyCode::Char('k') | KeyCode::Up => {
            if state.focus == Focus::List {
                state.move_selection(-1)
            } else {
                state.inspector_scroll = state.inspector_scroll.saturating_sub(1);
                state.inspector_horizontal = 0;
            }
        }
        KeyCode::Home => {
            if state.focus == Focus::List {
                state.move_to(0)
            } else {
                state.inspector_scroll = 0;
                state.inspector_horizontal = 0;
            }
        }
        KeyCode::End => {
            if state.focus == Focus::List {
                state.move_to(state.visible.len().saturating_sub(1))
            } else {
                state.inspector_scroll = state
                    .cached_lines
                    .len()
                    .saturating_sub(state.inspector_rows)
            }
        }
        KeyCode::PageDown => {
            if state.focus == Focus::List {
                state.move_selection(state.list_rows.max(1) as isize)
            } else {
                state.inspector_scroll = state
                    .inspector_scroll
                    .saturating_add(state.inspector_rows.max(1))
            }
        }
        KeyCode::PageUp => {
            if state.focus == Focus::List {
                state.move_selection(-(state.list_rows.max(1) as isize))
            } else {
                state.inspector_scroll = state
                    .inspector_scroll
                    .saturating_sub(state.inspector_rows.max(1))
            }
        }
        KeyCode::Char('n') => state.jump_match(true),
        KeyCode::Char('N') => state.jump_match(false),
        KeyCode::Char('r') => {
            state.raw_mode = !state.raw_mode;
            state.cached_entry = None;
            state.inspector_scroll = 0;
            state.inspector_horizontal = 0;
        }
        _ => {}
    }
    state.update_selected(file);
    state.clamp_inspector();
    Ok(false)
}

pub fn run(path: &Path, color: bool) -> Result<(), String> {
    let entries = index_file(path).map_err(|e| format!("{}: {e}", path.display()))?;
    let mut file = File::open(path).map_err(|e| format!("{}: {e}", path.display()))?;
    let mut state = State::new(path, entries, color);
    state.update_selected(&mut file);
    crossterm::terminal::enable_raw_mode().map_err(|e| e.to_string())?;
    let mut stdout = io::stdout();
    if let Err(error) = crossterm::execute!(stdout, crossterm::terminal::EnterAlternateScreen) {
        let _ = crossterm::terminal::disable_raw_mode();
        return Err(error.to_string());
    }
    let backend = CrosstermBackend::new(stdout);
    let terminal_result = Terminal::new(backend);
    let result = match terminal_result {
        Ok(mut terminal) => {
            let result = (|| -> io::Result<()> {
                loop {
                    let size = terminal.size()?;
                    let rows = size.height.saturating_sub(4).max(1) as usize;
                    state.list_rows = rows;
                    state.inspector_width =
                        (size.width as usize * 65 / 100).saturating_sub(8).max(18);
                    state.update_selected(&mut file);
                    state.cache_previews(&mut file)?;
                    terminal.draw(|frame| draw(frame, &mut state))?;
                    if event::poll(Duration::from_millis(150))?
                        && let Event::Key(key) = event::read()?
                        && handle(&mut state, &mut file, key).map_err(io::Error::other)?
                    {
                        break;
                    }
                }
                Ok(())
            })();
            drop(terminal);
            result.map_err(|e| e.to_string())
        }
        Err(error) => Err(error.to_string()),
    };
    let screen_restore = crossterm::execute!(
        io::stdout(),
        crossterm::terminal::LeaveAlternateScreen,
        crossterm::cursor::Show
    )
    .map_err(|e| e.to_string());
    let raw_restore = crossterm::terminal::disable_raw_mode().map_err(|e| e.to_string());
    result.and(screen_restore).and(raw_restore)
}

#[cfg(test)]
mod tests {
    use super::*;
    use ratatui::backend::TestBackend;

    #[test]
    fn index_filter_selection_and_inspector_search() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("records.jsonl");
        std::fs::write(&path, b"{\"kind\":\"boot\",\"value\":1}\n\n{broken}\n{\"kind\":\"request\",\"message\":\"Slow path\"}\n").unwrap();
        let entries = index_file(&path).unwrap();
        assert_eq!(entries.len(), 3);
        assert_eq!(entries[1].source_line, 3);
        assert_eq!(entries[2].source_line, 4);
        let mut file = File::open(&path).unwrap();
        let mut state = State::new(&path, entries, false);
        state.left_query = Some("REQUEST".into());
        state.filter(&mut file).unwrap();
        assert_eq!(state.visible, vec![2]);
        state.update_selected(&mut file);
        assert_eq!(state.cached_entry, Some(2));
        state.right_query = Some("slow path".into());
        state.rebuild_search_lines();
        assert_eq!(state.search_lines.len(), 1);
        assert_eq!(state.search_lines[0].0, 1);
        assert!(
            state.cached_lines[state.search_lines[0].0]
                .value
                .contains("Slow path")
        );
        state.left_query = None;
        state.filter(&mut file).unwrap();
        state.move_to(1);
        state.update_selected(&mut file);
        assert!(
            state
                .cached_error
                .as_deref()
                .unwrap()
                .contains("physical line 3")
        );
    }

    #[test]
    fn focused_search_prompt_filters_and_finds_inspector_lines() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("search.jsonl");
        std::fs::write(
            &path,
            b"{\"event\":\"boot\"}\n{\"event\":\"request\",\"note\":\"slow lane\"}\n",
        )
        .unwrap();
        let entries = index_file(&path).unwrap();
        let mut file = File::open(&path).unwrap();
        let mut state = State::new(&path, entries, false);
        assert!(
            !handle(
                &mut state,
                &mut file,
                KeyEvent::new(KeyCode::Char('/'), KeyModifiers::NONE)
            )
            .unwrap()
        );
        for c in "REQUEST".chars() {
            handle(
                &mut state,
                &mut file,
                KeyEvent::new(KeyCode::Char(c), KeyModifiers::NONE),
            )
            .unwrap();
        }
        handle(
            &mut state,
            &mut file,
            KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
        )
        .unwrap();
        assert_eq!(state.visible, vec![1]);
        handle(
            &mut state,
            &mut file,
            KeyEvent::new(KeyCode::Tab, KeyModifiers::NONE),
        )
        .unwrap();
        handle(
            &mut state,
            &mut file,
            KeyEvent::new(KeyCode::Char('/'), KeyModifiers::NONE),
        )
        .unwrap();
        for c in "SLOW LANE".chars() {
            handle(
                &mut state,
                &mut file,
                KeyEvent::new(KeyCode::Char(c), KeyModifiers::NONE),
            )
            .unwrap();
        }
        handle(
            &mut state,
            &mut file,
            KeyEvent::new(KeyCode::Enter, KeyModifiers::NONE),
        )
        .unwrap();
        assert_eq!(state.search_lines.len(), 1);
        assert_eq!(state.search_lines[0].0, 1);
        assert!(
            state.cached_lines[state.search_lines[0].0]
                .value
                .contains("slow lane")
        );
        handle(
            &mut state,
            &mut file,
            KeyEvent::new(KeyCode::Char('n'), KeyModifiers::NONE),
        )
        .unwrap();
        assert_eq!(state.inspector_scroll, 1);
    }

    #[test]
    fn long_inspector_line_is_horizontally_searchable() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("long.jsonl");
        let raw = format!("{{\"payload\":\"{}ENDMARK\"}}\n", "x".repeat(50_000));
        std::fs::write(&path, raw).unwrap();
        let entries = index_file(&path).unwrap();
        let mut file = File::open(&path).unwrap();
        let mut state = State::new(&path, entries, false);
        state.update_selected(&mut file);
        state.right_query = Some("ENDMARK".into());
        state.rebuild_search_lines();
        assert_eq!(state.search_lines.len(), 1);
        assert!(state.search_lines[0].0 > 100);
        assert!(state.search_lines[0].1 > 0);
        state.jump_match(true);
        let backend = TestBackend::new(100, 24);
        let mut terminal = Terminal::new(backend).unwrap();
        terminal.draw(|frame| draw(frame, &mut state)).unwrap();
        let rendered = terminal
            .backend()
            .buffer()
            .content
            .iter()
            .map(|cell| cell.symbol())
            .collect::<String>();
        assert!(rendered.contains("ENDMARK"));
    }

    #[test]
    fn unicode_case_expansion_maps_search_to_valid_source_offset() {
        let source = "😀 İ prefix NEEDLE";
        assert_eq!(
            folded_index(source, "needle"),
            Some(source.find("NEEDLE").unwrap())
        );
        assert_eq!(folded_index(source, "i\u{307}"), Some("😀 ".len()));

        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("unicode.jsonl");
        std::fs::write(&path, "{\"message\":\"😀 İ prefix NEEDLE\"}\n").unwrap();
        let entries = index_file(&path).unwrap();
        let mut file = File::open(&path).unwrap();
        let mut state = State::new(&path, entries, false);
        state.update_selected(&mut file);
        state.right_query = Some("needle".into());
        state.rebuild_search_lines();
        assert_eq!(state.search_lines.len(), 1);
        assert!(state.search_lines[0].1 > 0);
    }

    #[test]
    fn long_keys_and_numbers_wrap_within_inspector_width() {
        let key = "long_key_".repeat(12);
        let number = "1234567890".repeat(4);
        let raw = format!("{{\"{key}\":\"valuable NEEDLE\",\"n\":{number}}}");
        let value: serde_json::Value = serde_json::from_str(&raw).unwrap();
        let lines = display_fields(&value, 24);
        assert!(lines.iter().any(|line| line.kind == ValueKind::Key));
        assert!(
            lines
                .iter()
                .filter(|line| line.kind == ValueKind::Key)
                .all(|line| UnicodeWidthStr::width(line.label.as_str()) <= 12)
        );
        assert!(lines.iter().any(|line| line.value == "valuable NEEDLE"));
        assert!(
            lines
                .iter()
                .filter(|line| line.kind == ValueKind::Number)
                .count()
                >= 2
        );

        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("wide-key.jsonl");
        std::fs::write(&path, format!("{raw}\n")).unwrap();
        let entries = index_file(&path).unwrap();
        let mut file = File::open(&path).unwrap();
        let mut state = State::new(&path, entries, false);
        state.inspector_width = 24;
        state.update_selected(&mut file);
        state.right_query = Some("key_".into());
        state.rebuild_search_lines();
        assert!(
            state
                .search_lines
                .iter()
                .any(|(row, _)| { state.cached_lines[*row].kind == ValueKind::Key })
        );
        state.right_query = Some("needle".into());
        state.rebuild_search_lines();
        assert!(
            state
                .search_lines
                .iter()
                .any(|(row, _)| { state.cached_lines[*row].value.contains("NEEDLE") })
        );
    }

    #[test]
    fn test_backend_renders_master_and_detail() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("trace.jsonl");
        std::fs::write(&path, b"{\"event\":\"connected\",\"ok\":true}\n").unwrap();
        let entries = index_file(&path).unwrap();
        let mut file = File::open(&path).unwrap();
        let mut state = State::new(&path, entries, true);
        state.list_rows = 10;
        state.cache_previews(&mut file).unwrap();
        state.update_selected(&mut file);
        let backend = TestBackend::new(100, 24);
        let mut terminal = Terminal::new(backend).unwrap();
        terminal.draw(|frame| draw(frame, &mut state)).unwrap();
        let rendered = terminal
            .backend()
            .buffer()
            .content
            .iter()
            .map(|cell| cell.symbol())
            .collect::<String>();
        assert!(rendered.contains("Records 1/1"));
        assert!(rendered.contains("connected"));
        assert!(rendered.contains("Tab focus"));
    }
}
