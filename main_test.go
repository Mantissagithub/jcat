package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestFormatterPreservesOrderPrecisionAndLines(t *testing.T) {
	input := []byte("{\"z\":1234567890123456789012345678901234567890,\"a\":true}\r\n \r\n[\"x\",null]")
	var output bytes.Buffer
	if err := render(bytes.NewReader(input), &output, "stdin", false); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "record 2 | source line 3") {
		t.Fatalf("missing physical line: %s", got)
	}
	if strings.Index(got, `"z"`) > strings.Index(got, `"a"`) {
		t.Fatalf("object order changed: %s", got)
	}
	if !strings.Contains(got, "1234567890123456789012345678901234567890") {
		t.Fatalf("number precision changed: %s", got)
	}
}

func TestFormatterErrorNamesPhysicalLineAndColorMode(t *testing.T) {
	var output bytes.Buffer
	err := render(strings.NewReader("{}\n\n{bad}\n"), &output, "trace.jsonl", false)
	if err == nil || !strings.Contains(err.Error(), "trace.jsonl: invalid JSON at physical line 3") {
		t.Fatalf("unexpected error %v", err)
	}
	colored := colorJSON([]byte(`{"key":true}`))
	if !bytes.Contains(colored, []byte("\x1b[")) {
		t.Fatal("expected syntax colors")
	}
}

func TestFormatterHandlesReusedBufferAcrossVeryLongRecords(t *testing.T) {
	first := `{"payload":"` + strings.Repeat("a", 100_000) + `"}`
	second := `{"event":"after"}`
	input := first + "\n" + second
	var output bytes.Buffer
	if err := render(strings.NewReader(input), &output, "stdin", false); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, `"payload": "`+strings.Repeat("a", 100_000)+`"`) || !strings.Contains(got, `"event": "after"`) {
		t.Fatal("long record or final unterminated record was truncated")
	}
}

func TestFlagsAllowOptionsAfterFileAndHelp(t *testing.T) {
	o, help, err := parseArgs([]string{"trace.jsonl", "--color", "never", "--plain"})
	if err != nil || help || o.input != "trace.jsonl" || o.color != "never" || !o.plain {
		t.Fatalf("unexpected parse: %+v %v %v", o, help, err)
	}
	_, help, err = parseArgs([]string{"trace.jsonl", "--help"})
	if err != nil || !help {
		t.Fatalf("help not recognized: %v %v", help, err)
	}
}

func TestInspectorJumpsBetweenTraceMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	raw := `{"metadata":{"message":{"role":"ignored"}},"traces":[{"nodes":[{"message":{"role":"user","content":"` + strings.Repeat("wrapped content ", 100) + `"}},{"message":{"role":"tool","content":"done"}}]}]}`
	if err := os.WriteFile(path, []byte(raw+"\n{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	entries, err := indexFile(file)
	if err != nil {
		t.Fatal(err)
	}
	m := &model{file: file, entries: entries, visible: []int{0, 1}, cachedEntry: -1, width: 120, inspectorRows: 20, previews: map[int]string{}}
	m.loadSelected()
	if len(m.messages) != 2 {
		t.Fatalf("expected two trace messages, got %v", m.messages)
	}
	m.key("]")
	if m.focus != inspectorFocus || m.inspectorScroll != m.messages[0].row {
		t.Fatal("first jump did not focus the first message")
	}
	m.key("pgdown")
	if m.currentMessage() != 0 || m.inspectorScroll <= m.messages[0].row {
		t.Fatal("expected to be partway through the first message")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.currentMessage() != 0 {
		t.Fatal("resizing lost the current message")
	}
	m.key("]")
	if m.inspectorScroll != m.messages[1].row || !strings.Contains(m.rightPanel(50, 24), "Message 2/2 · tool") {
		t.Fatal("next jump did not show the final message at the top")
	}
	m.key("]")
	if m.inspectorScroll != m.messages[1].row {
		t.Fatal("jumping past the last message moved the inspector")
	}
	m.key("[")
	if m.inspectorScroll != m.messages[0].row {
		t.Fatal("previous jump did not return to the first message")
	}
	m.key("r")
	m.key("]")
	if !m.rawMode || len(m.messages) != 0 {
		t.Fatal("message jumps should only appear in Fields view")
	}
	m.key("r")
	m.key("tab")
	m.key("j")
	if len(m.messages) != 0 || m.selected != 1 {
		t.Fatal("message positions leaked into the next record")
	}
}

func TestIndexSummarySearchAndLongLineInspector(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.jsonl")
	raw := `{"event":"large_payload","level":"warn","payload":"` + strings.Repeat("x", 50000) + `ENDMARK"}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	entries, err := indexFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if got := summary([]byte(raw)); !strings.HasPrefix(got, "large_payload · warn") {
		t.Fatalf("summary priority wrong: %s", got)
	}
	m := &model{path: "records.jsonl", file: file, entries: entries, visible: []int{0}, selected: 0, focus: inspectorFocus, listRows: 10, inspectorRows: 18, cachedEntry: -1, previews: map[int]string{}, width: 120, height: 32, color: false}
	m.loadSelected()
	m.rightQuery = "ENDMARK"
	m.rebuildMatches()
	if len(m.matches) != 1 || m.matches[0].line < 100 {
		t.Fatalf("long-line match missing: %#v", m.matches)
	}
	m.jumpMatch(true)
	view := m.View().Content
	if !strings.Contains(view, "ENDMARK") {
		t.Fatal("search did not bring long-line match into view")
	}
	if lipgloss.Width(view) > 120 || lipgloss.Height(view) > 32 {
		t.Fatalf("view exceeds terminal: %dx%d", lipgloss.Width(view), lipgloss.Height(view))
	}
}

func TestFieldsPreserveOrderPrecisionAndEmptyKinds(t *testing.T) {
	raw := []byte(`{"later":123456789012345678901234567890,"first":{"items":["one",null,false,{}],"emptyArray":[],"emptyObject":{},"emptyString":""}}`)
	lines, err := displayFields(raw, 48)
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, line := range lines {
		got.WriteString(line.label + " " + line.value + "\n")
	}
	view := got.String()
	if strings.Index(view, "later") > strings.Index(view, "first") {
		t.Fatalf("object order changed: %s", view)
	}
	for _, want := range []string{"123456789012345678901234567890", "Object · 4 fields", "Array · 4 items", "Empty array", "Empty object", "Empty string", "Null", "false"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q from field view:\n%s", want, view)
		}
	}
	if strings.Contains(view, "{") || strings.Contains(view, "}") || strings.Contains(view, `"later"`) {
		t.Fatalf("field view contains JSON scaffolding: %s", view)
	}
}

func TestFieldWrappingAndSearchMapAcrossSegments(t *testing.T) {
	needle := "phrase crosses this soft wrap boundary"
	lines, err := displayFields([]byte(`{"message":"`+strings.Repeat("first words ", 8)+needle+` end"}`), 32)
	if err != nil {
		t.Fatal(err)
	}
	m := &model{cachedLines: lines, rightQuery: "soft wrap boundary"}
	m.rebuildMatches()
	if len(m.matches) != 1 || !strings.Contains(lines[m.matches[0].line].value, "soft") {
		t.Fatalf("wrapped text search was not mapped to its row: %#v", m.matches)
	}
	wide := displayFieldsMust([]byte(`{"key":"日本語の長いメッセージ"}`), 48)
	if strings.Join(lineTexts(wide), "") == "" {
		t.Fatal("wide Unicode value disappeared")
	}
	unicodeText := "😀 İ prefix NEEDLE"
	if got, want := foldedIndex(unicodeText, "needle"), strings.Index(unicodeText, "NEEDLE"); got != want {
		t.Fatalf("case-folded offset = %d, want source byte offset %d", got, want)
	}
	if got, want := foldedIndex(unicodeText, "İ"), strings.Index(unicodeText, "İ"); got != want {
		t.Fatalf("Unicode key offset = %d, want source byte offset %d", got, want)
	}
}

func TestLongKeysAndNumbersWrapWithinInspectorWidth(t *testing.T) {
	key := strings.Repeat("long_key_", 12)
	number := strings.Repeat("1234567890", 4)
	raw := []byte(`{"` + key + `":"valuable NEEDLE","n":` + number + `}`)
	lines, err := displayFields(raw, 24)
	if err != nil {
		t.Fatal(err)
	}
	keyRows := 0
	numberRows := 0
	for _, line := range lines {
		if line.kind == keyLine {
			keyRows++
			if lipgloss.Width(line.label) > 12 {
				t.Fatalf("wrapped key exceeds bounded width: %d", lipgloss.Width(line.label))
			}
		}
		if line.kind == numberLine {
			numberRows++
		}
	}
	if keyRows < 2 || numberRows < 2 {
		t.Fatalf("long fields did not wrap: key rows=%d number rows=%d", keyRows, numberRows)
	}
	m := &model{cachedLines: lines, rightQuery: "key_"}
	m.rebuildMatches()
	if len(m.matches) == 0 || lines[m.matches[0].line].kind != keyLine {
		t.Fatalf("long key is not searchable: %#v", m.matches)
	}
	m.rightQuery = "needle"
	m.rebuildMatches()
	if len(m.matches) == 0 || !strings.Contains(lines[m.matches[0].line].value, "NEEDLE") {
		t.Fatalf("value behind long key is not searchable: %#v", m.matches)
	}
}

func TestHuhSearchPromptLifecycleKeepsQAsText(t *testing.T) {
	m := &model{width: 100, height: 30, color: false}
	cmd := m.startSearch(listSearch)
	if cmd == nil || m.promptForm == nil {
		t.Fatal("Huh prompt did not initialize")
	}
	m.promptForm.WithWidth(72).WithHeight(8)
	view := m.View().Content
	if !strings.Contains(view, "Search records") || !strings.Contains(view, "Literal text") || !strings.Contains(view, "│") {
		t.Fatalf("scoped search form or input is not visible:\n%s", view)
	}
	if strings.Contains(view, "\x1b[38;") || strings.Contains(view, "\x1b[48;") {
		t.Fatalf("color escapes should be removed in --color never mode: %q", view)
	}
	updated, _ := m.Update(tea.KeyPressMsg(tea.Key{Code: 'q', Text: "q"}))
	if updated.(*model).done || m.promptValue != "q" || m.promptForm == nil {
		t.Fatalf("q should remain searchable: value=%q form=%v done=%v", m.promptValue, m.promptForm != nil, m.done)
	}
	_, _ = m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	if m.promptForm != nil || m.promptValue != "" || m.done {
		t.Fatal("Esc should cancel the prompt without quitting")
	}
}

func TestHuhPromptCommitsFastSlashTypeEnterBurst(t *testing.T) {
	m := &model{width: 100, height: 30, color: false}
	m.startSearch(inspectorSearch)
	for _, r := range "duration" {
		m.Update(tea.KeyPressMsg(tea.Key{Code: r, Text: string(r)}))
	}
	if m.promptForm == nil {
		t.Fatal("form completed before Enter")
	}
	_, cmd := m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter}))
	if cmd == nil {
		t.Fatal("Enter did not schedule form completion")
	}
	_, completion := m.Update(cmd())
	if completion != nil {
		// The completion command may be a follow-up bookkeeping message; run it
		// through Update just as Bubble Tea does for asynchronous form events.
		m.Update(completion())
	}
	if m.promptForm != nil {
		t.Fatalf("fast search sequence left form open with value %q", m.promptValue)
	}
	if m.rightQuery != "duration" {
		t.Fatalf("search was not applied: %q", m.rightQuery)
	}
}

func displayFieldsMust(raw []byte, width int) []displayLine {
	lines, err := displayFields(raw, width)
	if err != nil {
		panic(err)
	}
	return lines
}

func lineTexts(lines []displayLine) []string {
	var out []string
	for _, line := range lines {
		out = append(out, line.label+line.value)
	}
	return out
}

func TestInteractiveSearchPromptAcceptsQAndClearsNoResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.jsonl")
	if err := os.WriteFile(path, []byte("{\"event\":\"request\",\"payload\":\"tail marker\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := indexFile(f)
	if err != nil {
		t.Fatal(err)
	}
	m := &model{path: "data.jsonl", file: f, entries: entries, visible: []int{0}, selected: 0, focus: listFocus, listRows: 10, inspectorRows: 10, cachedEntry: -1, previews: map[int]string{}, width: 100, height: 30}
	m.startSearch(listSearch)
	m.Update(tea.KeyPressMsg(tea.Key{Code: 'q', Text: "q"}))
	if m.done || m.promptValue != "q" {
		t.Fatalf("q should be searchable, done=%v prompt=%q", m.done, m.promptValue)
	}
	m.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyEsc}))
	m.focus = inspectorFocus
	m.loadSelected()
	m.rightQuery = "missing"
	m.rebuildMatches()
	if len(m.matches) != 0 {
		t.Fatal("expected no matches")
	}
	if !strings.Contains(m.rightPanel(70, 20), "no matches") {
		t.Fatal("no-match state is not visible")
	}
	m.rightQuery = ""
	m.rebuildMatches()
	if len(m.matches) != 0 {
		t.Fatal("clear should recover")
	}
}

func TestColorAutoHonorsPresentEmptyNoColor(t *testing.T) {
	t.Setenv("TERM", "xterm")
	t.Setenv("NO_COLOR", "")
	if colorEnabled("auto", true) {
		t.Fatal("present NO_COLOR must disable auto colors")
	}
	if !colorEnabled("always", false) {
		t.Fatal("always must enable colors")
	}
	if colorEnabled("never", true) {
		t.Fatal("never must disable colors")
	}
}

func TestIndexedMalformedRecordsRemainSelected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(path, []byte("{}\n\n{bad}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := indexFile(f)
	if err != nil {
		t.Fatal(err)
	}
	m := &model{file: f, entries: entries, visible: []int{0, 1}, selected: 1, cachedEntry: -1}
	m.loadSelected()
	if !strings.Contains(m.cachedError, "physical line 3") {
		t.Fatalf("error not inspectable: %s", m.cachedError)
	}
}

func TestNoColorViewDoesNotUseColorAttributes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "empty-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := indexFile(f)
	if err != nil {
		t.Fatal(err)
	}
	m := &model{path: "empty.jsonl", file: f, entries: entries, visible: []int{}, selected: 0, focus: listFocus, width: 120, height: 32, listRows: 26, inspectorRows: 26, previews: map[int]string{}, color: false}
	view := m.View().Content
	if strings.Contains(view, "\x1b[38;") || strings.Contains(view, "\x1b[48;") {
		t.Fatal("color attributes emitted with colors disabled")
	}
}
