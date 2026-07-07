package issuesync

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Atlassian Document Format (ADF) is the rich-text JSON Jira stores for issue
// descriptions and comment bodies. Issue sync keeps Multica's canonical text as
// markdown, so the Jira provider converts ADF→markdown on the way in and
// markdown→ADF on the way out. Both directions are best-effort: they cover the
// common block/inline constructs and degrade gracefully (unknown nodes drop to
// their text content; un-parseable markdown becomes a plain paragraph).

// adfNode is the generic shape of every ADF node: a type, optional attrs,
// optional child content, and — for leaf text nodes — the literal text plus any
// marks. Unknown nodes are still walked so future ADF features don't break.
type adfNode struct {
	Type    string          `json:"type"`
	Attrs   map[string]any  `json:"attrs,omitempty"`
	Content []adfNode       `json:"content,omitempty"`
	Text    string          `json:"text,omitempty"`
	Marks   []adfMark       `json:"marks,omitempty"`
}

type adfMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// ADFToMarkdown converts an ADF document to markdown. nil/null/empty input
// returns "". Supports the common block types (paragraph, heading, bulletList,
// orderedList, codeBlock, blockquote) and inline marks (strong/em/code/strike/
// link), mentions, and inlineCards. Unknown nodes contribute their text content
// when present.
func ADFToMarkdown(adf json.RawMessage) string {
	if len(adf) == 0 {
		return ""
	}
	// ADF may arrive as a doc object, a bare block, or an array of blocks.
	var doc adfNode
	if err := json.Unmarshal(adf, &doc); err == nil {
		switch {
		case doc.Type == "doc":
			return renderADFBlocks(doc.Content)
		case doc.Type != "":
			return renderADFBlocks([]adfNode{doc})
		case len(doc.Content) > 0:
			return renderADFBlocks(doc.Content)
		}
		return ""
	}
	// Fall back to an array of blocks.
	var blocks []adfNode
	if err := json.Unmarshal(adf, &blocks); err == nil {
		return renderADFBlocks(blocks)
	}
	// Fall back to a plain string (some payloads store raw text).
	var s string
	if err := json.Unmarshal(adf, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(adf))
}

// renderADFBlocks joins top-level blocks with a blank line — matching how
// markdown parsers separate paragraphs/headings/lists, so round-tripping a
// multi-block document is stable.
func renderADFBlocks(blocks []adfNode) string {
	parts := make([]string, 0, len(blocks))
	for _, n := range blocks {
		parts = append(parts, renderADFBlock(n))
	}
	return strings.TrimRight(strings.Join(parts, "\n\n"), "\n")
}

func renderADFBlock(n adfNode) string {
	switch n.Type {
	case "doc":
		return renderADFBlocks(n.Content)
	case "paragraph":
		return renderADFInline(n.Content)
	case "heading":
		return strings.Repeat("#", adfHeadingLevel(n.Attrs)) + " " + renderADFInline(n.Content)
	case "bulletList":
		return renderADFList(n.Content, "- ")
	case "orderedList":
		return renderADFList(n.Content, "%d. ")
	case "codeBlock":
		return "```\n" + renderADFInline(n.Content) + "\n```"
	case "blockquote":
		var inner []string
		for _, c := range n.Content {
			if s := renderADFBlock(c); s != "" {
				inner = append(inner, s)
			}
		}
		return "> " + strings.Join(inner, "\n> ")
	case "panel":
		// Jira panel (info/note/warning) — render inner blocks inline.
		return renderADFBlocks(n.Content)
	case "rule":
		return "---"
	default:
		// Unknown block: extract whatever inline text it carries.
		return renderADFInline(n.Content)
	}
}

// renderADFList emits consecutive list items; each listItem's first paragraph
// becomes the item text and any nested list is appended on following lines.
func renderADFList(items []adfNode, marker string) string {
	var lines []string
	idx := 0
	for _, item := range items {
		if item.Type != "listItem" {
			continue
		}
		idx++
		var text strings.Builder
		var nested []string
		for _, c := range item.Content {
			switch c.Type {
			case "paragraph":
				text.WriteString(renderADFInline(c.Content))
			case "bulletList":
				nested = append(nested, renderADFList(c.Content, "- "))
			case "orderedList":
				nested = append(nested, renderADFList(c.Content, "%d. "))
			default:
				if s := renderADFBlock(c); s != "" {
					nested = append(nested, s)
				}
			}
		}
		prefix := marker
		if strings.Contains(marker, "%d") {
			prefix = fmt.Sprintf(marker, idx)
		}
		lines = append(lines, prefix+text.String())
		lines = append(lines, prefixNestedList(nested)...)
	}
	return strings.Join(lines, "\n")
}

// prefixNestedList indents nested list lines so they render as sub-items.
func prefixNestedList(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = "  " + l
	}
	return out
}

// renderADFInline concatenates inline node content with marks applied.
func renderADFInline(nodes []adfNode) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text", "":
			b.WriteString(applyADFMarks(n.Text, n.Marks))
		case "mention":
			b.WriteString(adfAttrString(n.Attrs, "text", adfAttrString(n.Attrs, "id", "")))
		case "inlineCard":
			if u := adfAttrString(n.Attrs, "url", ""); u != "" {
				b.WriteString(u)
			}
		case "hardBreak":
			b.WriteString("\n")
		default:
			// Unknown inline: recurse into its content for any text.
			if len(n.Content) > 0 {
				b.WriteString(renderADFInline(n.Content))
			}
		}
	}
	return b.String()
}

// applyADFMarks wraps text in the markdown sigils for its marks. code suppresses
// other inline formatting (matching CommonMark precedence).
func applyADFMarks(text string, marks []adfMark) string {
	if text == "" || len(marks) == 0 {
		return text
	}
	bold, italic, code, strike := false, false, false, false
	linkURL := ""
	for _, m := range marks {
		switch m.Type {
		case "strong":
			bold = true
		case "em":
			italic = true
		case "code":
			code = true
		case "strike":
			strike = true
		case "link":
			linkURL = adfAttrString(m.Attrs, "href", "")
		}
	}
	if code {
		return "`" + text + "`"
	}
	out := text
	if bold {
		out = "**" + out + "**"
	}
	if italic {
		out = "*" + out + "*"
	}
	if strike {
		out = "~~" + out + "~~"
	}
	if linkURL != "" {
		out = "[" + out + "](" + linkURL + ")"
	}
	return out
}

func adfHeadingLevel(attrs map[string]any) int {
	if v, ok := attrs["level"].(float64); ok {
		if n := int(v); n >= 1 && n <= 6 {
			return n
		}
	}
	return 1
}

func adfAttrString(attrs map[string]any, key, fallback string) string {
	if v, ok := attrs[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// MarkdownToADF converts markdown into a minimal ADF document. It is a
// best-effort line-oriented parser: headings (#..######), bullet (-/*) and
// ordered (N.) lists, blockquotes (>), and fenced code blocks (```) map to ADF
// blocks; everything else becomes a paragraph. Inline **bold**, *italic*, and
// `code` spans become marked text nodes. The output is always a valid ADF doc.
func MarkdownToADF(md string) json.RawMessage {
	lines := strings.Split(md, "\n")
	content := make([]map[string]any, 0)
	for i := 0; i < len(lines); {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		if strings.HasPrefix(line, "```") {
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			if i < len(lines) {
				i++ // consume closing fence
			}
			content = append(content, adfCodeBlock(strings.Join(code, "\n")))
			continue
		}
		if level, rest, ok := adfHeadingLine(line); ok {
			content = append(content, adfHeading(level, rest))
			i++
			continue
		}
		if isBulletLine(line) {
			var items []string
			for i < len(lines) && isBulletLine(strings.TrimSpace(lines[i])) {
				items = append(items, strings.TrimSpace(strings.TrimSpace(lines[i])[1:]))
				i++
			}
			content = append(content, adfList("bulletList", items))
			continue
		}
		if _, ok := adfNumberedLine(line); ok {
			var items []string
			for i < len(lines) {
				cur, ok := adfNumberedLine(strings.TrimSpace(lines[i]))
				if !ok {
					break
				}
				items = append(items, cur)
				i++
			}
			content = append(content, adfList("orderedList", items))
			continue
		}
		if strings.HasPrefix(line, ">") {
			content = append(content, adfBlockquote(strings.TrimSpace(strings.TrimPrefix(line, ">"))))
			i++
			continue
		}
		content = append(content, adfParagraph(line))
		i++
	}
	if len(content) == 0 {
		// An empty doc still needs a paragraph to be valid ADF.
		content = []map[string]any{adfParagraph("")}
	}
	doc := map[string]any{
		"type":    "doc",
		"version": 1,
		"content": content,
	}
	raw, _ := json.Marshal(doc)
	return raw
}

// ── ADF node builders ───────────────────────────────────────────────────────

func adfParagraph(text string) map[string]any {
	return map[string]any{"type": "paragraph", "content": parseMarkdownInline(text)}
}

func adfHeading(level int, text string) map[string]any {
	return map[string]any{
		"type":    "heading",
		"attrs":   map[string]any{"level": level},
		"content": parseMarkdownInline(text),
	}
}

func adfCodeBlock(text string) map[string]any {
	node := map[string]any{"type": "codeBlock"}
	if text != "" {
		node["content"] = []map[string]any{{"type": "text", "text": text}}
	}
	return node
}

func adfList(listType string, items []string) map[string]any {
	content := make([]map[string]any, 0, len(items))
	for _, it := range items {
		content = append(content, map[string]any{
			"type":    "listItem",
			"content": []map[string]any{adfParagraph(it)},
		})
	}
	return map[string]any{"type": listType, "content": content}
}

func adfBlockquote(text string) map[string]any {
	return map[string]any{
		"type":    "blockquote",
		"content": []map[string]any{adfParagraph(text)},
	}
}

// ── Inline markdown parser ──────────────────────────────────────────────────

// parseMarkdownInline turns a single line of markdown into ADF text nodes with
// marks. Recognizes `code`, **bold**, and *italic* spans; everything else is a
// plain text node. Adjacent plain text is kept as one node.
func parseMarkdownInline(text string) []map[string]any {
	if text == "" {
		return []map[string]any{}
	}
	var nodes []map[string]any
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			nodes = append(nodes, map[string]any{"type": "text", "text": buf.String()})
			buf.Reset()
		}
	}
	for i := 0; i < len(text); {
		c := text[i]
		// `code`
		if c == '`' {
			if end := strings.IndexByte(text[i+1:], '`'); end >= 0 {
				flush()
				nodes = append(nodes, markedTextNode(text[i+1:i+1+end], "code"))
				i = i + 1 + end + 1
				continue
			}
		}
		// **bold**
		if c == '*' && i+1 < len(text) && text[i+1] == '*' {
			if end := strings.Index(text[i+2:], "**"); end >= 0 {
				flush()
				inner := text[i+2 : i+2+end]
				for _, child := range parseMarkdownInline(inner) {
					nodes = append(nodes, addADFMark(child, "strong"))
				}
				i = i + 2 + end + 2
				continue
			}
		}
		// *italic*
		if c == '*' {
			if end := strings.IndexByte(text[i+1:], '*'); end >= 0 {
				flush()
				inner := text[i+1 : i+1+end]
				for _, child := range parseMarkdownInline(inner) {
					nodes = append(nodes, addADFMark(child, "em"))
				}
				i = i + 1 + end + 1
				continue
			}
		}
		buf.WriteByte(c)
		i++
	}
	flush()
	return nodes
}

func markedTextNode(text, mark string) map[string]any {
	return map[string]any{
		"type":  "text",
		"text":  text,
		"marks": []map[string]any{{"type": mark}},
	}
}

// addADFMark appends a mark to an existing text node, preserving any marks the
// node already carries (so **_bold italic_** nests correctly).
func addADFMark(node map[string]any, mark string) map[string]any {
	marks, _ := node["marks"].([]map[string]any)
	node["marks"] = append(marks, map[string]any{"type": mark})
	return node
}

// ── Line classifiers ────────────────────────────────────────────────────────

func adfHeadingLine(line string) (level int, rest string, ok bool) {
	level = 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	// Require a space after the #'s (or end-of-line) to avoid matching "#tag".
	rest = line[level:]
	if rest != "" && !strings.HasPrefix(rest, " ") {
		return 0, "", false
	}
	return level, strings.TrimSpace(rest), true
}

func isBulletLine(line string) bool {
	if line == "" {
		return false
	}
	if line[0] != '-' && line[0] != '*' {
		return false
	}
	// Must be followed by content (a lone "-" is a rule, not a bullet).
	return len(line) > 1 && (line[1] == ' ' || line[1] == '\t')
}

// adfNumberedLine recognizes "N." ordered-list markers and returns the item
// text (without the marker).
func adfNumberedLine(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != '.' {
		return "", false
	}
	rest := strings.TrimSpace(line[i+1:])
	if rest == "" {
		return "", false
	}
	return rest, true
}
