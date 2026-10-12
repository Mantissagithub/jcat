package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const previewLimit = 72
const summaryCacheLimit = 128

type entry struct {
	offset     int64
	length     int
	sourceLine int
	preview    string
}
type focus int

const (
	listFocus focus = iota
	inspectorFocus
)

type searchTarget int

const (
	listSearch searchTarget = iota
	inspectorSearch
)

type match struct{ line, column int }

type model struct {
	path                        string
	file                        *os.File
	entries                     []entry
	visible                     []int
	selected                    int
	focus                       focus
	leftQuery, rightQuery       string
	promptForm                  *huh.Form
	promptValue                 string
	promptTarget                searchTarget
	help                        bool
	helpScroll                  int
	width, height               int
	inspectorScroll, horizontal int
	inspectorRows, listRows     int
	cachedWidth                 int
	rawMode                     bool
	cachedRaw                   bool
	cachedEntry                 int
	cachedLines                 []displayLine
	cachedError                 string
	matches                     []match
	matchCursor                 int
	previews                    map[int]string
	color                       bool
	done                        bool
}

func indexFile(file *os.File) ([]entry, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return nil, err
	}
	r := bufio.NewReader(file)
	var entries []entry
	offset := int64(0)
	lineNo := 0
	var raw []byte
	for {
		var eof bool
		var err error
		raw, eof, err = readLine(r, raw)
		if len(raw) == 0 && eof {
			break
		}
		if err != nil {
			return nil, err
		}
		lineNo++
		start := offset
		offset += int64(len(raw))
		content := bytes.TrimRight(raw, "\r\n")
		if len(bytes.TrimSpace(content)) > 0 {
			preview := safePreview(content)
			entries = append(entries, entry{start, len(raw), lineNo, preview})
		}
		if eof {
			break
		}
	}
	return entries, nil
}

func summary(raw []byte) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return safePreview(raw)
	}
	pick := func(keys ...string) string {
		for _, key := range keys {
			raw, ok := obj[key]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return s
			}
			if len(raw) > 0 && raw[0] != '{' && raw[0] != '[' {
				return string(raw)
			}
		}
		return ""
	}
	primary := pick("event", "type", "role", "name", "method")
	if primary == "" {
		return safePreview(raw)
	}
	parts := []string{primary}
	if x := pick("message", "content", "path", "service", "status", "duration_ms"); x != "" {
		parts = append(parts, x)
	}
	if x := pick("level", "severity"); x != "" {
		parts = append(parts, x)
	}
	return truncateRunes(strings.Join(parts, " · "), previewLimit)
}
func safePreview(raw []byte) string {
	var b strings.Builder
	count := 0
	for len(raw) > 0 && count < previewLimit {
		r, size := utf8.DecodeRune(raw)
		if size < 1 {
			size = 1
		}
		raw = raw[size:]
		if r < 0x20 {
			continue
		}
		b.WriteRune(r)
		count++
	}
	if len(raw) > 0 {
		b.WriteRune('…')
	}
	return b.String()
}
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= n-1 {
			b.WriteRune('…')
			return b.String()
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}
func (m *model) cacheVisiblePreviews() {
	first := m.selected - (m.listRows - 1)
	if first < 0 {
		first = 0
	}
	end := first + m.listRows
	if end > len(m.visible) {
		end = len(m.visible)
	}
	for _, i := range m.visible[first:end] {
		if _, ok := m.previews[i]; ok {
			continue
		}
		if len(m.previews) >= summaryCacheLimit {
			m.previews = make(map[int]string)
		}
		e := m.entries[i]
		raw := make([]byte, e.length)
		if _, err := m.file.ReadAt(raw, e.offset); err != nil {
			m.previews[i] = e.preview
		} else {
			m.previews[i] = summary(bytes.TrimRight(raw, "\r\n"))
		}
	}
}
func (m *model) loadSelected() {
	idx, ok := m.selectedEntry()
	if !ok {
		m.cachedEntry = -1
		m.cachedLines = nil
		m.cachedError = ""
		m.matches = nil
		return
	}
	width := max(18, m.width*65/100-10)
	if idx == m.cachedEntry && width == m.cachedWidth && m.rawMode == m.cachedRaw {
		return
	}
	e := m.entries[idx]
	m.cachedLines = nil
	raw := make([]byte, e.length)
	if _, err := m.file.ReadAt(raw, e.offset); err != nil {
		m.cachedError = "Read error: " + err.Error()
		m.cachedLines = nil
	} else {
		raw = bytes.TrimRight(raw, "\r\n")
		if m.rawMode {
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, raw, "", "  "); err != nil {
				m.cachedError = fmt.Sprintf("Invalid JSON at physical line %d: %v", e.sourceLine, err)
				m.cachedLines = nil
			} else {
				m.cachedError = ""
				for _, line := range strings.Split(pretty.String(), "\n") {
					m.cachedLines = append(m.cachedLines, displayLine{value: line, kind: rawLine})
				}
			}
		} else if lines, err := displayFields(raw, width); err != nil {
			m.cachedError = fmt.Sprintf("Invalid JSON at physical line %d: %v", e.sourceLine, err)
			m.cachedLines = nil
		} else {
			m.cachedError = ""
			m.cachedLines = lines
		}
	}
	m.cachedEntry = idx
	m.cachedWidth = width
	m.cachedRaw = m.rawMode
	m.rebuildMatches()
}
func (m *model) selectedEntry() (int, bool) {
	if m.selected < 0 || m.selected >= len(m.visible) {
		return 0, false
	}
	return m.visible[m.selected], true
}
func (m *model) filter() error {
	query := strings.ToLower(m.leftQuery)
	m.visible = m.visible[:0]
	var raw []byte
	for i, e := range m.entries {
		if query == "" {
			m.visible = append(m.visible, i)
			continue
		}
		if cap(raw) < e.length {
			raw = make([]byte, e.length)
		} else {
			raw = raw[:e.length]
		}
		if _, err := m.file.ReadAt(raw, e.offset); err != nil {
			return fmt.Errorf("search read error at physical line %d: %w", e.sourceLine, err)
		}
		if strings.Contains(strings.ToLower(string(raw)), query) {
			m.visible = append(m.visible, i)
		}
	}
	m.selected = 0
	m.inspectorScroll = 0
	m.horizontal = 0
	m.cachedEntry = -1
	m.matches = nil
	m.loadSelected()
	return nil
}
func (m *model) rebuildMatches() {
	m.matches = nil
	m.matchCursor = 0
	if m.rightQuery == "" {
		return
	}
	q := strings.ToLower(m.rightQuery)
	seen := make(map[int]bool)
	for i, line := range m.cachedLines {
		if line.kind == rawLine {
			if byte := foldedIndex(line.value, q); byte >= 0 {
				m.matches = append(m.matches, match{i, lipgloss.Width(line.value[:byte])})
			}
			continue
		}
		if seen[line.sourceID] {
			continue
		}
		seen[line.sourceID] = true
		if line.kind == keyLine {
			if keyAt := foldedIndex(line.searchText, q); keyAt >= line.start && keyAt < line.end {
				column := line.depth*2 + lipgloss.Width(line.label[:keyAt-line.start])
				m.matches = append(m.matches, match{i, column})
			}
			continue
		}
		if line.label != "" {
			if keyAt := foldedIndex(line.label, q); keyAt >= 0 {
				m.matches = append(m.matches, match{i, line.depth*2 + lipgloss.Width(line.label[:keyAt])})
				continue
			}
		}
		byte := foldedIndex(line.searchText, q)
		if byte >= 0 {
			row := i
			for j := i; j < len(m.cachedLines); j++ {
				candidate := m.cachedLines[j]
				if candidate.sourceID != line.sourceID {
					break
				}
				if candidate.start <= byte && byte < candidate.end || (byte == len(line.searchText) && candidate.end == len(line.searchText)) {
					row = j
					break
				}
			}
			segment := m.cachedLines[row]
			column := segment.depth*2 + segment.labelWidth + 2 + lipgloss.Width(line.searchText[segment.start:byte])
			if segment.label != "" {
				column = segment.depth*2 + segment.labelWidth + 2 + lipgloss.Width(line.searchText[segment.start:byte])
			}
			m.matches = append(m.matches, match{row, column})
		}
	}
	if len(m.matches) > 0 {
		m.inspectorScroll = m.matches[0].line
		m.horizontal = m.matches[0].column
	} else {
		m.inspectorScroll = 0
		m.horizontal = 0
	}
}

func (m *model) startSearch(target searchTarget) tea.Cmd {
	m.promptTarget = target
	m.promptValue = ""
	scope := "fields"
	if target == listSearch {
		scope = "records"
	} else if m.rawMode {
		scope = "raw JSON"
	}
	input := huh.NewInput().Title("Search " + scope).Description("Literal text · Enter apply · Esc cancel").Value(&m.promptValue)
	theme := huh.ThemeCharm
	if !m.color {
		theme = huh.ThemeBase
	}
	formWidth := min(72, max(20, m.width-12))
	m.promptForm = huh.NewForm(huh.NewGroup(input)).WithTheme(huh.ThemeFunc(theme)).WithWidth(formWidth).WithHeight(8)
	return m.promptForm.Init()
}

func (m *model) updateSearchForm(msg tea.Msg) tea.Cmd {
	updated, cmd := m.promptForm.Update(msg)
	if form, ok := updated.(*huh.Form); ok {
		m.promptForm = form
	}
	if m.promptForm != nil && m.promptForm.State == huh.StateCompleted {
		m.commitSearch()
	}
	return cmd
}

func (m *model) commitSearch() {
	m.promptForm = nil
	if m.promptTarget == listSearch {
		m.leftQuery = m.promptValue
		if err := m.filter(); err != nil {
			m.cachedError = err.Error()
		}
	} else {
		m.rightQuery = m.promptValue
		m.rebuildMatches()
	}
	m.promptValue = ""
}

func (m *model) jumpMatch(forward bool) {
	if len(m.matches) == 0 {
		return
	}
	if forward {
		m.matchCursor = (m.matchCursor + 1) % len(m.matches)
	} else {
		m.matchCursor = (m.matchCursor + len(m.matches) - 1) % len(m.matches)
	}
	m.inspectorScroll = m.matches[m.matchCursor].line
	m.horizontal = m.matches[m.matchCursor].column
}
func (m *model) Init() tea.Cmd { return nil }
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.listRows = max(1, m.height-4)
		m.inspectorRows = max(1, m.height-4)
		if m.promptForm != nil {
			m.promptForm.WithWidth(min(72, max(20, m.width-12))).WithHeight(8)
			cmd := m.updateSearchForm(msg)
			m.loadSelected()
			return m, cmd
		}
		m.loadSelected()
		m.clampInspector()
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.promptForm == nil && !m.help && msg.String() == "/" {
			target := inspectorSearch
			if m.focus == listFocus {
				target = listSearch
			}
			return m, m.startSearch(target)
		}
		if m.promptForm != nil {
			if msg.String() == "esc" {
				m.promptForm = nil
				m.promptValue = ""
				return m, nil
			}
			cmd := m.updateSearchForm(msg)
			return m, cmd
		}
		m.key(msg.String())
		if m.done {
			return m, tea.Quit
		}
	default:
		if m.promptForm != nil {
			return m, m.updateSearchForm(msg)
		}
	}
	return m, nil
}
func (m *model) key(key string) {
	if key == "ctrl+c" || key == "q" {
		m.done = true
		return
	}
	if m.help {
		switch key {
		case "esc", "?":
			m.help = false
		case "j", "down":
			m.helpScroll++
		case "k", "up":
			m.helpScroll = max(0, m.helpScroll-1)
		case "pgdown":
			m.helpScroll += 8
		case "pgup":
			m.helpScroll = max(0, m.helpScroll-8)
		}
		return
	}
	switch key {
	case "q":
		m.done = true
	case "?":
		m.help = true
	case "tab":
		if m.focus == listFocus {
			m.focus = inspectorFocus
		} else {
			m.focus = listFocus
		}
	case "r":
		m.rawMode = !m.rawMode
		m.cachedEntry = -1
		m.loadSelected()
	case "esc":
		if m.focus == listFocus {
			m.leftQuery = ""
			if err := m.filter(); err != nil {
				m.cachedError = err.Error()
			}
		} else {
			m.rightQuery = ""
			m.rebuildMatches()
		}
	case "j", "down":
		if m.focus == listFocus {
			m.move(1)
		} else {
			m.inspectorScroll++
			m.horizontal = 0
		}
	case "k", "up":
		if m.focus == listFocus {
			m.move(-1)
		} else {
			m.inspectorScroll = max(0, m.inspectorScroll-1)
			m.horizontal = 0
		}
	case "h", "left":
		if m.focus == inspectorFocus {
			m.horizontal = max(0, m.horizontal-20)
		}
	case "l", "right":
		if m.focus == inspectorFocus {
			m.horizontal += 20
		}
	case "home":
		if m.focus == listFocus {
			m.selected = 0
			m.horizontal = 0
			m.loadSelected()
		} else {
			m.inspectorScroll = 0
			m.horizontal = 0
		}
	case "end":
		if m.focus == listFocus {
			m.selected = max(0, len(m.visible)-1)
			m.horizontal = 0
			m.loadSelected()
		} else {
			m.inspectorScroll = max(0, len(m.cachedLines)-m.inspectorRows)
		}
	case "pgdown":
		if m.focus == listFocus {
			m.move(m.listRows)
		} else {
			m.inspectorScroll += m.inspectorRows
		}
	case "pgup":
		if m.focus == listFocus {
			m.move(-m.listRows)
		} else {
			m.inspectorScroll = max(0, m.inspectorScroll-m.inspectorRows)
		}
	case "n":
		m.jumpMatch(true)
	case "N":
		m.jumpMatch(false)
	}
	m.clampInspector()
	m.cacheVisiblePreviews()
	return
}
func (m *model) move(delta int) {
	if len(m.visible) == 0 {
		return
	}
	m.selected = max(0, min(len(m.visible)-1, m.selected+delta))
	m.inspectorScroll = 0
	m.horizontal = 0
	m.loadSelected()
}
func (m *model) clampInspector() {
	m.inspectorScroll = max(0, min(m.inspectorScroll, max(0, len(m.cachedLines)-m.inspectorRows)))
}
func (m *model) View() tea.View {
	if m.width < 50 || m.height < 10 {
		return tea.View{Content: "Resize terminal to at least 50 columns × 10 rows (q quits).", AltScreen: true}
	}
	if m.listRows == 0 {
		m.listRows = max(1, m.height-4)
	}
	if m.inspectorRows == 0 {
		m.inspectorRows = max(1, m.height-4)
	}
	m.cacheVisiblePreviews()
	leftWidth := m.width * 35 / 100
	rightWidth := m.width - leftWidth
	bodyHeight := m.height - 2
	left := m.leftPanel(leftWidth, bodyHeight)
	right := m.rightPanel(rightWidth, bodyHeight)
	mode := "FIELDS"
	if m.rawMode {
		mode = "RAW JSON"
	}
	header := titleStyle(m.color).Render(" jcat  "+m.path) + "  " + badgeStyle(m.color).Render(mode)
	panes := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	footer := fmt.Sprintf(" %d/%d rows · %d matches │ Tab focus · j/k move · h/l scroll · r fields/raw · / search · ? help · q quit", len(m.visible), len(m.entries), len(m.matches))
	if m.help {
		footer = " Help: j/k or PgUp/PgDn scroll · q quit · Esc close"
	}
	content := header + "\n" + panes + "\n" + footer
	if m.promptForm != nil {
		panelWidth := min(76, max(24, m.width-4))
		formView := m.promptForm.View()
		if !m.color {
			formView = ansi.Strip(formView)
		}
		panel := lipgloss.NewStyle().Width(panelWidth).Border(lipgloss.RoundedBorder()).BorderForeground(panelColor(true, m.color)).Padding(0, 1).Render(formView)
		content = lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panel)
	}
	if m.help {
		help := "KEYS\n  Tab             Switch panels\n  j/k, ↑/↓        Select record or scroll inspector\n  PageUp/PageDown, Home/End\n  /               Search focused panel with Huh input\n  Enter           Apply search; Esc cancels prompt\n  Esc             Clear the active search\n  n/N             Next/previous matching inspector row\n  h/l or ←/→     Scroll horizontally in raw mode\n  r               Toggle Fields / Raw JSON\n  q / Ctrl-C      Quit\n\nSEARCH MANUAL\n  Case-insensitive literal substring, not regex or JSON field query.\n  Records searches full raw JSONL records. Fields searches displayed keys and values.\n  Raw JSON searches the selected pretty-printed record.\n  Enter applies; Esc cancels or clears. No-results can be cleared with Esc."
		hlines := strings.Split(help, "\n")
		from := min(m.helpScroll, len(hlines))
		to := min(from+max(1, bodyHeight-2), len(hlines))
		panel := lipgloss.NewStyle().Width(m.width-8).Height(bodyHeight).Border(lipgloss.RoundedBorder()).BorderForeground(panelColor(true, m.color)).Padding(0, 1).Render(strings.Join(hlines[from:to], "\n"))
		content = header + "\n" + panel + "\n" + footer
	}
	v := tea.View{Content: content, AltScreen: true}
	return v
}
func (m *model) leftPanel(width, height int) string {
	inner := max(1, width-4)
	contentRows := max(1, height-2)
	rows := make([]string, 0, contentRows)
	first := max(0, m.selected-(contentRows-1))
	end := min(first+contentRows-1, len(m.visible))
	title := fmt.Sprintf("Records %d/%d", len(m.visible), len(m.entries))
	if m.leftQuery != "" {
		title += " /" + m.leftQuery
	}
	rows = append(rows, titleStyle(m.color).Render(truncateRunes(title, inner)))
	if len(m.visible) == 0 {
		rows = append(rows, "No records match this filter")
	} else {
		for _, idx := range m.visible[first:end] {
			preview := m.previews[idx]
			line := fmt.Sprintf("#%d %s", idx+1, preview)
			line = truncateRunes(line, inner)
			if idx == m.visible[m.selected] {
				if m.color {
					line = lipgloss.NewStyle().Background(lipgloss.Color("#55DDF0")).Foreground(lipgloss.Color("#111111")).Bold(true).Render(padRunes(line, inner))
				} else {
					line = lipgloss.NewStyle().Reverse(true).Bold(true).Render(padRunes(line, inner))
				}
			}
			rows = append(rows, line)
		}
	}
	return lipgloss.NewStyle().Width(width).Height(height).Border(lipgloss.RoundedBorder()).BorderForeground(panelColor(m.focus == listFocus, m.color)).Padding(0, 1).Render(strings.Join(rows, "\n"))
}
func (m *model) rightPanel(width, height int) string {
	inner := max(1, width-4)
	rows := make([]string, 0, max(1, height-2))
	idx, ok := m.selectedEntry()
	title := "Fields · no records"
	if m.rawMode {
		title = "Raw JSON · no records"
	}
	if ok {
		mode := "Fields"
		if m.rawMode {
			mode = "Raw JSON"
		}
		title = fmt.Sprintf("%s · record %d · source line %d", mode, idx+1, m.entries[idx].sourceLine)
		if m.rightQuery != "" {
			if len(m.matches) == 0 {
				title += " · no matches"
			} else {
				title += " · matches"
			}
		}
	}
	rows = append(rows, titleStyle(m.color).Render(truncateRunes(title, inner)))
	if m.cachedError != "" {
		rows = append(rows, truncateRunes(m.cachedError, inner))
	} else if len(m.cachedLines) == 0 {
		if len(m.entries) == 0 {
			rows = append(rows, "Empty file")
		} else if len(m.visible) == 0 {
			rows = append(rows, "No records match this filter")
		}
	} else {
		end := min(m.inspectorScroll+max(0, height-3), len(m.cachedLines))
		for i := m.inspectorScroll; i < end; i++ {
			item := m.cachedLines[i]
			line := item.value
			if item.kind != rawLine {
				indent := strings.Repeat(" ", item.depth*2)
				padding := max(0, item.labelWidth-lipgloss.Width(item.label))
				label := item.label
				if label != "" {
					label = labelStyle(m.color).Render(label)
				}
				if item.kind == keyLine {
					line = indent + label
				} else {
					value := valueStyle(item.kind, m.color).Render(line)
					line = indent + label + strings.Repeat(" ", padding) + "  " + value
				}
			} else {
				line = runeSlice(line, m.horizontal, inner-6)
				line = syntax(line, m.color)
			}
			prefix := ""
			if m.rawMode {
				prefix = fmt.Sprintf("%4d │ ", i+1)
			}
			styled := prefix + line
			lineMatched := false
			for _, hit := range m.matches {
				if hit.line == i {
					lineMatched = true
					break
				}
			}
			if m.rightQuery != "" && lineMatched {
				if m.color {
					styled = lipgloss.NewStyle().Background(lipgloss.Color("#444444")).Bold(true).Render(line)
				} else {
					styled = lipgloss.NewStyle().Reverse(true).Render(line)
				}
			}
			rows = append(rows, styled)
		}
	}
	return lipgloss.NewStyle().Width(width).Height(height).Border(lipgloss.RoundedBorder()).BorderForeground(panelColor(m.focus == inspectorFocus, m.color)).Padding(0, 1).Render(strings.Join(rows, "\n"))
}
func titleStyle(color bool) lipgloss.Style {
	if color {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("#55DDF0")).Bold(true)
	}
	return lipgloss.NewStyle().Bold(true)
}
func badgeStyle(color bool) lipgloss.Style {
	style := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	if color {
		return style.Foreground(lipgloss.Color("#111111")).Background(lipgloss.Color("#55DDF0"))
	}
	return style.Reverse(true)
}
func labelStyle(color bool) lipgloss.Style {
	style := lipgloss.NewStyle().Bold(true)
	if color {
		return style.Foreground(lipgloss.Color("#55DDF0"))
	}
	return style
}
func valueStyle(kind lineKind, color bool) lipgloss.Style {
	style := lipgloss.NewStyle()
	if !color {
		return style
	}
	switch kind {
	case sectionLine:
		return style.Foreground(lipgloss.Color("#F4B860")).Bold(true)
	case stringLine:
		return style.Foreground(lipgloss.Color("#A8E6CF"))
	case numberLine:
		return style.Foreground(lipgloss.Color("#FFD166"))
	case boolLine:
		return style.Foreground(lipgloss.Color("#C792EA")).Bold(true)
	case nullLine:
		return style.Foreground(lipgloss.Color("#8B95A5")).Italic(true)
	default:
		return style
	}
}
func panelColor(focus, colorEnabled bool) color.Color {
	if !colorEnabled {
		return nil
	}
	if focus && colorEnabled {
		return lipgloss.Color("#55DDF0")
	}
	return lipgloss.Color("#666666")
}
func padRunes(s string, n int) string {
	r := utf8.RuneCountInString(s)
	if r >= n {
		return s
	}
	return s + strings.Repeat(" ", n-r)
}
func runeSlice(s string, skip, take int) string {
	if skip < 0 {
		skip = 0
	}
	if take <= 0 {
		return ""
	}
	var b strings.Builder
	seen := 0
	for _, r := range s {
		if seen >= skip && seen < skip+take {
			b.WriteRune(r)
		}
		seen++
		if seen >= skip+take {
			break
		}
	}
	return b.String()
}
func syntax(s string, color bool) string {
	if !color {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '"' {
			start := i
			i++
			for i < len(s) {
				if s[i] == '\\' {
					i += 2
					continue
				}
				i++
				if s[i-1] == '"' {
					break
				}
			}
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			c := "#A8E6CF"
			if j < len(s) && s[j] == ':' {
				c = "#55DDF0"
			}
			out.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c)).Render(s[start:min(i, len(s))]))
			continue
		}
		if strings.ContainsRune("-0123456789", rune(s[i])) {
			start := i
			i++
			for i < len(s) && strings.ContainsRune("0123456789.eE+-", rune(s[i])) {
				i++
			}
			out.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD166")).Render(s[start:i]))
			continue
		}
		if s[i] == 't' || s[i] == 'f' || s[i] == 'n' {
			n := 4
			if strings.HasPrefix(s[i:], "false") {
				n = 5
			}
			out.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#C792EA")).Render(s[i:min(i+n, len(s))]))
			i += n
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size < 1 {
			size = 1
		}
		out.WriteString(s[i : i+size])
		i += size
	}
	return out.String()
}

func runTUI(path string, color bool) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	entries, err := indexFile(file)
	if err != nil {
		file.Close()
		return fmt.Errorf("%s: %w", path, err)
	}
	m := &model{path: filepath.Base(path), file: file, entries: entries, selected: 0, focus: listFocus, cachedEntry: -1, listRows: 18, inspectorRows: 18, previews: make(map[int]string), color: color}
	m.visible = make([]int, len(entries))
	for i := range entries {
		m.visible[i] = i
	}
	m.loadSelected()
	m.cacheVisiblePreviews()
	p := tea.NewProgram(m)
	_, err = p.Run()
	file.Close()
	return err
}
