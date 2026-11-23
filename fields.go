package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type nodeKind uint8

const (
	objectNode nodeKind = iota
	arrayNode
	stringNode
	numberNode
	boolNode
	nullNode
)

type jsonNode struct {
	kind   nodeKind
	text   string
	fields []jsonField
	items  []*jsonNode
}

type jsonField struct {
	key   string
	value *jsonNode
}

type lineKind uint8

const (
	sectionLine lineKind = iota
	stringLine
	numberLine
	boolLine
	nullLine
	keyLine
	rawLine
)

type displayLine struct {
	depth       int
	label       string
	labelWidth  int
	value       string
	styled      string
	kind        lineKind
	searchText  string
	sourceID    int
	start       int
	end         int
	messageRole string
}

type fieldPath uint8

const (
	otherField fieldPath = iota
	recordObject
	tracesArray
	traceObject
	nodesArray
	traceNode
	traceMessage
)

func (p fieldPath) field(key string) fieldPath {
	switch {
	case p == recordObject && key == "traces":
		return tracesArray
	case p == traceObject && key == "nodes":
		return nodesArray
	case p == traceNode && key == "message":
		return traceMessage
	default:
		return otherField
	}
}

func (p fieldPath) item() fieldPath {
	switch p {
	case tracesArray:
		return traceObject
	case nodesArray:
		return traceNode
	default:
		return otherField
	}
}

type wrapSegment struct {
	text       string
	start, end int
}

func decodeNode(dec *json.Decoder) (*jsonNode, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &jsonNode{kind: objectNode}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return nil, err
				}
				child, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.fields = append(n.fields, jsonField{keyToken.(string), child})
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &jsonNode{kind: arrayNode}
			for dec.More() {
				child, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, child)
			}
			_, err := dec.Token()
			return n, err
		}
	case string:
		return &jsonNode{kind: stringNode, text: v}, nil
	case json.Number:
		return &jsonNode{kind: numberNode, text: v.String()}, nil
	case bool:
		return &jsonNode{kind: boolNode, text: fmt.Sprint(v)}, nil
	case nil:
		return &jsonNode{kind: nullNode, text: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected JSON token %v", token)
}

func parseNode(raw []byte) (*jsonNode, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := decodeNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return n, nil
}

func displayFields(raw []byte, width int) ([]displayLine, error) {
	n, err := parseNode(raw)
	if err != nil {
		return nil, err
	}
	lines := make([]displayLine, 0, 32)
	width = max(18, width)
	sequence := 0
	if n.kind == objectNode {
		if len(n.fields) == 0 {
			lines = append(lines, displayLine{value: "Empty object", kind: sectionLine})
		} else {
			appendFields(&lines, n.fields, 0, width, &sequence, recordObject)
		}
	} else if n.kind == arrayNode {
		appendValue(&lines, "Value", n, 0, 8, width, &sequence, otherField)
	} else {
		appendValue(&lines, "Value", n, 0, 8, width, &sequence, otherField)
	}
	return lines, nil
}

func appendFields(lines *[]displayLine, fields []jsonField, depth, width int, sequence *int, path fieldPath) {
	labelWidth := 0
	for _, field := range fields {
		labelWidth = max(labelWidth, lipgloss.Width(safeText(field.key)))
	}
	labelWidth = min(labelWidth, max(12, width/2))
	for _, field := range fields {
		label := safeText(field.key)
		if lipgloss.Width(label) > labelWidth {
			for _, segment := range wrapSegments(label, labelWidth) {
				*sequence++
				*lines = append(*lines, displayLine{depth: depth, label: segment.text, labelWidth: lipgloss.Width(segment.text), kind: keyLine, searchText: label, sourceID: *sequence, start: segment.start, end: segment.end})
			}
			appendValue(lines, "", field.value, depth, 0, width, sequence, path.field(field.key))
		} else {
			appendValue(lines, label, field.value, depth, labelWidth, width, sequence, path.field(field.key))
		}
	}
}

func appendValue(lines *[]displayLine, label string, n *jsonNode, depth, labelWidth, width int, sequence *int, path fieldPath) {
	indent := depth * 2
	valueWidth := max(12, width-indent-labelWidth-4)
	*sequence++
	id := *sequence
	switch n.kind {
	case objectNode:
		if len(n.fields) == 0 {
			*lines = append(*lines, displayLine{depth: depth, label: label, labelWidth: labelWidth, value: "Empty object", kind: sectionLine, searchText: "Empty object", sourceID: id, end: len("Empty object")})
			return
		}
		value := fmt.Sprintf("Object · %d fields", len(n.fields))
		line := displayLine{depth: depth, label: label, labelWidth: labelWidth, value: value, kind: sectionLine, searchText: value, sourceID: id, end: len(value)}
		if path == traceMessage {
			for _, field := range n.fields {
				if field.key == "role" && field.value.kind == stringNode {
					line.messageRole = strings.Join(strings.Fields(safeText(field.value.text)), " ")
					break
				}
			}
		}
		*lines = append(*lines, line)
		appendFields(lines, n.fields, depth+1, width, sequence, path)
	case arrayNode:
		if len(n.items) == 0 {
			*lines = append(*lines, displayLine{depth: depth, label: label, labelWidth: labelWidth, value: "Empty array", kind: sectionLine, searchText: "Empty array", sourceID: id, end: len("Empty array")})
			return
		}
		value := fmt.Sprintf("Array · %d items", len(n.items))
		*lines = append(*lines, displayLine{depth: depth, label: label, labelWidth: labelWidth, value: value, kind: sectionLine, searchText: value, sourceID: id, end: len(value)})
		itemWidth := 2
		for i := range n.items {
			itemWidth = max(itemWidth, len(fmt.Sprint(i+1)))
		}
		for i, item := range n.items {
			appendValue(lines, fmt.Sprintf("%0*d", itemWidth, i+1), item, depth+1, itemWidth, width, sequence, path.item())
		}
	case stringNode:
		if n.text == "" {
			*lines = append(*lines, displayLine{depth: depth, label: label, labelWidth: labelWidth, value: "Empty string", kind: stringLine, searchText: "Empty string", sourceID: id, end: len("Empty string")})
			return
		}
		if rendered := renderField(n.text); len(rendered) > 0 {
			appendRenderedField(lines, label, depth, labelWidth, valueWidth, id, rendered)
			return
		}
		text := safeText(n.text)
		first := true
		for _, segment := range wrapSegments(text, valueWidth) {
			rowLabel := ""
			if first {
				rowLabel = label
				first = false
			}
			*lines = append(*lines, displayLine{depth: depth, label: rowLabel, labelWidth: labelWidth, value: segment.text, kind: stringLine, searchText: text, sourceID: id, start: segment.start, end: segment.end})
		}
	case numberNode:
		first := true
		for _, segment := range wrapSegments(n.text, valueWidth) {
			rowLabel := ""
			if first {
				rowLabel = label
				first = false
			}
			*lines = append(*lines, displayLine{depth: depth, label: rowLabel, labelWidth: labelWidth, value: segment.text, kind: numberLine, searchText: n.text, sourceID: id, start: segment.start, end: segment.end})
		}
	case boolNode:
		*lines = append(*lines, displayLine{depth: depth, label: label, labelWidth: labelWidth, value: n.text, kind: boolLine, searchText: n.text, sourceID: id, end: len(n.text)})
	case nullNode:
		*lines = append(*lines, displayLine{depth: depth, label: label, labelWidth: labelWidth, value: "Null", kind: nullLine, searchText: "Null", sourceID: id, end: len("Null")})
	}
}

func appendRenderedField(lines *[]displayLine, label string, depth, labelWidth, width, id int, rows []string) {
	plain := make([]string, len(rows))
	for i, row := range rows {
		plain[i] = ansi.Strip(row)
	}
	search := strings.Join(plain, "\n")
	offset := 0
	for i, row := range rows {
		segments := wrapSegments(plain[i], width)
		styled := wrapStyles(row, segments)
		for j, segment := range segments {
			*lines = append(*lines, displayLine{
				depth: depth, label: label, labelWidth: labelWidth, value: segment.text,
				styled: styled[j], kind: stringLine, searchText: search,
				sourceID: id, start: offset + segment.start, end: offset + segment.end,
			})
			label = ""
		}
		offset += len(plain[i]) + 1
	}
}

func safeText(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			out.WriteRune(r)
		case r == '\t':
			out.WriteString(`\t`)
		case r == '\r':
			out.WriteString(`\r`)
		case unicode.IsControl(r):
			out.WriteString(fmt.Sprintf(`\u%04x`, r))
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func wrapSegments(s string, width int) []wrapSegment {
	var lines []wrapSegment
	for paragraphStart := 0; paragraphStart <= len(s); {
		paragraphEnd := paragraphStart
		for paragraphEnd < len(s) && s[paragraphEnd] != '\n' {
			cluster, _ := ansi.FirstGraphemeCluster(s[paragraphEnd:], ansi.GraphemeWidth)
			paragraphEnd += len(cluster)
		}
		if paragraphStart == paragraphEnd {
			lines = append(lines, wrapSegment{"", paragraphStart, paragraphEnd})
		} else {
			for start := paragraphStart; start < paragraphEnd; {
				cursor, cells, lastBreak := start, 0, -1
				for cursor < paragraphEnd {
					cluster, clusterWidth := ansi.FirstGraphemeCluster(s[cursor:paragraphEnd], ansi.GraphemeWidth)
					if len(cluster) == 0 || (cells+clusterWidth > width && cursor > start) {
						break
					}
					cursor += len(cluster)
					cells += clusterWidth
					if r, _ := utf8.DecodeRuneInString(cluster); unicode.IsSpace(r) {
						lastBreak = cursor
					}
				}
				end := cursor
				if cursor < paragraphEnd && lastBreak > start {
					end = lastBreak
				}
				if end <= start {
					cluster, _ := ansi.FirstGraphemeCluster(s[start:paragraphEnd], ansi.GraphemeWidth)
					end = start + len(cluster)
				}
				lines = append(lines, wrapSegment{s[start:end], start, end})
				start = end
			}
		}
		if paragraphEnd == len(s) {
			break
		}
		paragraphStart = paragraphEnd + 1
	}
	return lines
}

// foldedIndex returns an index into the original UTF-8 string while matching
// case-insensitively. Lowercase runes can change byte length, so keep an
// output-byte to source-byte map for correct search and display offsets.
func foldedIndex(text, query string) int {
	var folded strings.Builder
	var sourceOffsets []int
	for offset, r := range text {
		lower := strings.ToLower(string(r))
		folded.WriteString(lower)
		for range []byte(lower) {
			sourceOffsets = append(sourceOffsets, offset)
		}
	}
	index := strings.Index(folded.String(), strings.ToLower(query))
	if index < 0 {
		return -1
	}
	return sourceOffsets[index]
}
