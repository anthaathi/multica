package issuesync

import (
	"encoding/json"
	"strings"
	"testing"
)

// roundTrip converts markdown → ADF → markdown and reports the result.
func roundTrip(t *testing.T, md string) string {
	t.Helper()
	adf := MarkdownToADF(md)
	got := ADFToMarkdown(adf)
	return got
}

func TestADFEmptyAndNull(t *testing.T) {
	if got := ADFToMarkdown(nil); got != "" {
		t.Fatalf("ADFToMarkdown(nil) = %q, want empty", got)
	}
	if got := ADFToMarkdown(json.RawMessage("null")); got != "" {
		t.Fatalf("ADFToMarkdown(null) = %q, want empty", got)
	}
	// Empty markdown still yields a valid (empty-paragraph) doc.
	adf := MarkdownToADF("")
	var doc struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(adf, &doc); err != nil {
		t.Fatalf("empty markdown did not produce valid ADF JSON: %v", err)
	}
	if doc.Type != "doc" || doc.Version != 1 {
		t.Fatalf("empty ADF doc = %+v, want type=doc version=1", doc)
	}
}

func TestADFParagraphRoundTrip(t *testing.T) {
	cases := []string{
		"Hello world",
		"A plain paragraph with several words.",
	}
	for _, md := range cases {
		if got := roundTrip(t, md); got != md {
			t.Fatalf("round trip paragraph %q → %q", md, got)
		}
	}
}

func TestADFInlineMarksRoundTrip(t *testing.T) {
	cases := []string{
		"**bold text**",
		"*italic text*",
		"`code text`",
		"**bold** and *italic* and `code`",
	}
	for _, md := range cases {
		if got := roundTrip(t, md); got != md {
			t.Fatalf("round trip inline %q → %q", md, got)
		}
	}
}

func TestADFHeadingRoundTrip(t *testing.T) {
	for _, level := range []int{1, 2, 3, 4, 5, 6} {
		md := strings.Repeat("#", level) + " Title " + string(rune('A'-1+level))
		md = strings.Repeat("#", level) + " Title"
		if got := roundTrip(t, md); got != md {
			t.Fatalf("round trip heading %q → %q", md, got)
		}
	}
}

func TestADFBulletListRoundTrip(t *testing.T) {
	md := "- alpha\n- beta\n- gamma"
	if got := roundTrip(t, md); got != md {
		t.Fatalf("round trip bullet list %q → %q", md, got)
	}
}

func TestADFNumberedListRoundTrip(t *testing.T) {
	md := "1. first\n2. second\n3. third"
	if got := roundTrip(t, md); got != md {
		t.Fatalf("round trip numbered list %q → %q", md, got)
	}
}

// TestMarkdownToADFOrderedListSchemaValid guards against emitting a node type
// that is not in the ADF schema. A prior version wrote "numberedList", which
// round-trips with this code's own reader but is rejected by Jira's REST API
// with 400 "not valid Atlassian Document Format content" — breaking every
// outbound create/update whose description contained a numbered list. The only
// schema-valid ordered-list type is "orderedList".
func TestMarkdownToADFOrderedListSchemaValid(t *testing.T) {
	adf := MarkdownToADF("1. a\n2. b")
	var doc struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(adf, &doc); err != nil {
		t.Fatalf("invalid ADF JSON: %v", err)
	}
	if len(doc.Content) != 1 {
		t.Fatalf("expected 1 block, got %d", len(doc.Content))
	}
	if got := doc.Content[0]["type"]; got != "orderedList" {
		t.Fatalf("ordered-list node type = %v, want orderedList (the only schema-valid type)", got)
	}
}

// TestADFToMarkdownOrderedList verifies the inbound direction against a real
// Jira ADF payload (which uses "orderedList"), independent of the writer.
func TestADFToMarkdownOrderedList(t *testing.T) {
	adf := json.RawMessage(`{
		"type": "doc",
		"version": 1,
		"content": [
			{"type": "orderedList", "content": [
				{"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "one"}]}]},
				{"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "two"}]}]}
			]}
		]
	}`)
	want := "1. one\n2. two"
	if got := ADFToMarkdown(adf); got != want {
		t.Fatalf("ADFToMarkdown orderedList = %q, want %q", got, want)
	}
}

func TestADFCodeBlockRoundTrip(t *testing.T) {
	md := "```\nfunc main() {}\nprint('hi')\n```"
	if got := roundTrip(t, md); got != md {
		t.Fatalf("round trip code block %q → %q", md, got)
	}
}

func TestADFBlockquoteRoundTrip(t *testing.T) {
	md := "> quoted text"
	if got := roundTrip(t, md); got != md {
		t.Fatalf("round trip blockquote %q → %q", md, got)
	}
}

func TestADFMultiBlockRoundTrip(t *testing.T) {
	md := "# Heading One\n\nA paragraph here.\n\n- item one\n- item two"
	if got := roundTrip(t, md); got != md {
		t.Fatalf("round trip multi-block %q → %q", md, got)
	}
}

// TestADFToMarkdownFromKnownADF verifies the ADF→markdown direction against a
// hand-written ADF doc, independent of the markdown parser.
func TestADFToMarkdownFromKnownADF(t *testing.T) {
	adf := json.RawMessage(`{
		"type": "doc",
		"version": 1,
		"content": [
			{"type": "heading", "attrs": {"level": 2}, "content": [{"type": "text", "text": "My Heading"}]},
			{"type": "paragraph", "content": [
				{"type": "text", "text": "bold "},
				{"type": "text", "text": "word", "marks": [{"type": "strong"}]},
				{"type": "text", "text": " and "},
				{"type": "text", "text": "linked", "marks": [{"type": "link", "attrs": {"href": "https://x.example"}}]}
			]},
			{"type": "bulletList", "content": [
				{"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "one"}]}]},
				{"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "two"}]}]}
			]}
		]
	}`)
	want := "## My Heading\n\nbold **word** and [linked](https://x.example)\n\n- one\n- two"
	if got := ADFToMarkdown(adf); got != want {
		t.Fatalf("ADFToMarkdown known doc =\n%q\nwant\n%q", got, want)
	}
}

// TestADFToMarkdownMentionAndInlineCard verifies mention + inlineCard nodes emit
// their human text / url.
func TestADFToMarkdownMentionAndInlineCard(t *testing.T) {
	adf := json.RawMessage(`{
		"type": "doc",
		"version": 1,
		"content": [
			{"type": "paragraph", "content": [
				{"type": "mention", "attrs": {"id": "5b10ac", "text": "@Alice"}},
				{"type": "text", "text": " see "},
				{"type": "inlineCard", "attrs": {"url": "https://app.example/x"}}
			]}
		]
	}`)
	want := "@Alice see https://app.example/x"
	if got := ADFToMarkdown(adf); got != want {
		t.Fatalf("ADFToMarkdown mention/card = %q, want %q", got, want)
	}
}

// TestMarkdownToADFStructure checks the generated ADF node tree for a heading +
// list, confirming marks land on the right text nodes.
func TestMarkdownToADFStructure(t *testing.T) {
	adf := MarkdownToADF("# T\n\n**b**")
	var doc struct {
		Type    string              `json:"type"`
		Content []map[string]any    `json:"content"`
	}
	if err := json.Unmarshal(adf, &doc); err != nil {
		t.Fatalf("invalid ADF JSON: %v", err)
	}
	if doc.Type != "doc" {
		t.Fatalf("type = %q, want doc", doc.Type)
	}
	if len(doc.Content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(doc.Content))
	}
	// First block: heading level 1.
	if doc.Content[0]["type"] != "heading" {
		t.Fatalf("block 0 type = %v, want heading", doc.Content[0]["type"])
	}
	// Second block: paragraph with one bold text node.
	para, _ := doc.Content[1]["content"].([]any)
	if len(para) != 1 {
		t.Fatalf("paragraph content len = %d, want 1", len(para))
	}
	node, _ := para[0].(map[string]any)
	if node["text"] != "b" {
		t.Fatalf("text node = %v, want b", node["text"])
	}
	marks, _ := node["marks"].([]any)
	if len(marks) != 1 {
		t.Fatalf("marks len = %d, want 1 (strong)", len(marks))
	}
	m, _ := marks[0].(map[string]any)
	if m["type"] != "strong" {
		t.Fatalf("mark type = %v, want strong", m["type"])
	}
}
