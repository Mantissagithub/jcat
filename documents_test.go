package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestInspectorRendersMarkdownAndXMLFields(t *testing.T) {
	raw := `{"message":"# Report\n\n**Searchable** text\n\n- First","payload":"<root><node a=\"a>b\"/><p>See <b><c/></b> now &amp; forever.</p><keep xml:space=\"preserve\"><x/><y/></keep></root>","broken":"<a><b></a>","plain":"keep this text"}`
	path := filepath.Join(t.TempDir(), "trace.jsonl")
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
	m := &model{file: file, entries: entries, visible: []int{0}, cachedEntry: -1, width: 160, color: true}
	m.loadSelected()
	var rendered strings.Builder
	for _, row := range m.cachedLines {
		if row.styled != "" && ansi.Strip(row.styled) != row.value {
			t.Fatalf("styled text differs from searchable text: %q vs %q", row.styled, row.value)
		}
		rendered.WriteString(row.value + "\n")
	}
	got := rendered.String()
	for _, want := range []string{"Report", "Searchable text", "• First", "\n  <node", "<p>See <b><c/></b> now &amp; forever.</p>", "><x/><y/></keep>", "<a><b></a>", "keep this text"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q from %q", want, got)
		}
	}
	if strings.Contains(got, "# Report") || strings.Contains(got, "**Searchable**") {
		t.Fatalf("Markdown markers appeared in rendered fields: %s", got)
	}
	m.rightQuery = "searchable text"
	m.rebuildMatches()
	if len(m.matches) != 1 {
		t.Fatalf("rendered field was not searchable: %#v", m.matches)
	}
	m.width = 54
	m.loadSelected()
	for _, row := range m.cachedLines {
		if row.styled != "" && ansi.Strip(row.styled) != row.value {
			t.Fatalf("wrapped style changed visible text: %q vs %q", row.styled, row.value)
		}
	}
	m.rawMode = true
	m.loadSelected()
	if !strings.Contains(m.cachedLines[1].value, `# Report\n\n**Searchable**`) {
		t.Fatalf("raw view did not preserve Markdown: %#v", m.cachedLines)
	}
}
