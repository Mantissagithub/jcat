package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"

	"charm.land/glamour/v2"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

var (
	markdownSyntax = regexp.MustCompile("(?m)^ {0,3}(#{1,6}[ \t]|`{3,}|~{3,}|>[ \t]|[-+*][ \t]|[0-9]+[.)][ \t])|\\*\\*[^*\\n]+\\*\\*|__[^_\\n]+__|\\*[^*\\n]+\\*|`[^`\\n]+`|\\[[^\\]\\n]+\\]\\(")
	sgr            = regexp.MustCompile(`\x1b\[[0-9;:]*m`)
)

func renderField(text string) []string {
	if strings.HasPrefix(strings.TrimSpace(text), "<") {
		if rendered, err := renderXML(text); err == nil {
			return strings.Split(rendered, "\n")
		}
		// Leave malformed XML visible in the original string.
		return nil
	}
	if !markdownSyntax.MatchString(text) {
		return nil
	}
	style := styles.DarkStyleConfig
	style.Document.Margin = nil
	style.Document.Color = nil
	renderer, err := glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(0))
	if err != nil {
		return nil
	}
	rendered, err := renderer.Render(safeText(strings.ReplaceAll(text, "\r\n", "\n")))
	if err != nil {
		return nil
	}

	// Decoded entities can introduce control characters. Keep only styling escapes.
	var out strings.Builder
	start := 0
	for _, match := range sgr.FindAllStringIndex(rendered, -1) {
		out.WriteString(safeText(ansi.Strip(rendered[start:match[0]])))
		out.WriteString(rendered[match[0]:match[1]])
		start = match[1]
	}
	out.WriteString(safeText(ansi.Strip(rendered[start:])))
	lines := strings.Split(out.String(), "\n")
	for i, line := range lines {
		width := ansi.StringWidth(strings.TrimRight(ansi.Strip(line), " "))
		lines[i] = ansi.Truncate(line, width, "")
	}
	for len(lines) > 0 && ansi.Strip(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && ansi.Strip(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Wrap using the same byte offsets as inspector search. Replay the active
// styles on continuation rows, since each row renders independently.
func wrapStyles(row string, segments []wrapSegment) []string {
	styled := make([]string, len(segments))
	if len(segments) == 1 {
		styled[0] = row
		return styled
	}
	escapes := sgr.FindAllStringIndex(row, -1)
	plain, encoded := 0, 0
	active := ""
	for i, segment := range segments {
		var out strings.Builder
		out.WriteString(active)
		for plain < segment.end {
			end := encoded + segment.end - plain
			if len(escapes) > 0 && escapes[0][0] < end {
				start, after := escapes[0][0], escapes[0][1]
				out.WriteString(row[encoded:after])
				style := row[start:after]
				if style == "\x1b[m" || style == "\x1b[0m" {
					active = ""
				} else {
					active += style
				}
				plain += start - encoded
				encoded = after
				escapes = escapes[1:]
			} else {
				out.WriteString(row[encoded:end])
				plain = segment.end
				encoded = end
			}
		}
		if active != "" {
			out.WriteString("\x1b[0m")
		}
		styled[i] = out.String()
	}
	return styled
}

// Keep original markup and text. Reindent only containers whose children
// are separated by whitespace and which do not request xml:space preservation.
func renderXML(text string) (string, error) {
	type container struct {
		parent                       int
		preserve, hasText, hasMarkup bool
	}
	type piece struct {
		text          string
		parent, depth int
		markup        bool
	}
	containers := []container{{}}
	stack := []int{0}
	var pieces []piece
	decoder := xml.NewDecoder(strings.NewReader(text))
	roots := 0
	for {
		start := decoder.InputOffset()
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		depth := len(stack) - 1
		parent := stack[depth]
		raw := text[start:decoder.InputOffset()]
		part := piece{text: raw, parent: parent, depth: depth, markup: true}
		switch t := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			containers[parent].hasMarkup = true
			next := container{parent: parent, preserve: containers[parent].preserve}
			for _, attr := range t.Attr {
				if attr.Name.Space == "http://www.w3.org/XML/1998/namespace" && attr.Name.Local == "space" {
					next.preserve = attr.Value != "default"
				}
			}
			stack = append(stack, len(containers))
			containers = append(containers, next)
		case xml.EndElement:
			stack = stack[:depth]
			if raw == "" { // The decoder also emits an end token for <empty/>.
				continue
			}
			part.depth--
		case xml.CharData:
			part.markup = false
			if strings.Trim(raw, " \t\r\n") != "" {
				if depth == 0 {
					return "", fmt.Errorf("text outside the root element")
				}
				containers[parent].hasText = true
			}
		default:
			containers[parent].hasMarkup = true
		}
		pieces = append(pieces, part)
	}
	if roots == 0 {
		return "", fmt.Errorf("expected an element")
	}
	for i := 1; i < len(containers); i++ {
		containers[i].hasText = containers[i].hasText || containers[containers[i].parent].hasText
	}

	var out strings.Builder
	for _, part := range pieces {
		parent := containers[part.parent]
		if !parent.preserve && !parent.hasText && parent.hasMarkup {
			if !part.markup {
				continue
			}
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(strings.Repeat("  ", part.depth))
		}
		text := safeText(part.text)
		if part.markup {
			text = "\x1b[36m" + strings.ReplaceAll(text, "\n", "\x1b[0m\n\x1b[36m") + "\x1b[0m"
		}
		out.WriteString(text)
	}
	return out.String(), nil
}
