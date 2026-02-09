package render

import (
	"strings"
	"testing"
)

func TestNormalizeMode(t *testing.T) {
	// NormalizeMode now always returns markdown_stream
	if got := NormalizeMode(""); got != ModeMarkdownStream {
		t.Fatalf("NormalizeMode(\"\") = %q, want %q", got, ModeMarkdownStream)
	}
	if got := NormalizeMode("plain"); got != ModeMarkdownStream {
		t.Fatalf("NormalizeMode(\"plain\") = %q, want %q", got, ModeMarkdownStream)
	}
	if got := NormalizeMode("markdown_final"); got != ModeMarkdownStream {
		t.Fatalf("NormalizeMode(\"markdown_final\") = %q, want %q", got, ModeMarkdownStream)
	}
	if got := NormalizeMode("markdown_stream"); got != ModeMarkdownStream {
		t.Fatalf("NormalizeMode(\"markdown_stream\") = %q, want %q", got, ModeMarkdownStream)
	}
	if got := NormalizeMode("unknown"); got != ModeMarkdownStream {
		t.Fatalf("NormalizeMode(\"unknown\") = %q, want %q", got, ModeMarkdownStream)
	}
}

func TestRenderer_RenderMode(t *testing.T) {
	// Renderer now always uses HTML rendering (markdown_stream mode)
	r := New("markdown_stream")
	streaming := r.Render("**hello**", true)
	if !streaming.UseHTML {
		t.Fatalf("renderer should use HTML while streaming")
	}
	if !strings.Contains(streaming.Text, "<b>hello</b>") {
		t.Fatalf("expected bold conversion, got %q", streaming.Text)
	}

	final := r.Render("**hello**", false)
	if !final.UseHTML {
		t.Fatalf("renderer should use HTML for final rendering")
	}
	if !strings.Contains(final.Text, "<b>hello</b>") {
		t.Fatalf("expected bold conversion, got %q", final.Text)
	}
}

func TestMarkdownToTelegramHTML(t *testing.T) {
	input := "# Title\nA **bold** and ~~strike~~ text with [link](https://example.com?q=1&k=2)\n`code`\n***both***"
	got := MarkdownToTelegramHTML(input)

	expectContains := []string{
		"<b>Title</b>", // Heading should be bold without emoji prefix
		"<b>bold</b>",
		"<s>strike</s>",
		`<a href="https://example.com?q=1&amp;k=2">link</a>`,
		"<code>code</code>",
		"<b><i>both</i></b>",
	}
	for _, want := range expectContains {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
}

func TestMarkdownToTelegramHTML_FencePreserved(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")\n```"
	got := MarkdownToTelegramHTML(input)

	if strings.Contains(got, "<b>") || strings.Contains(got, "<a ") {
		t.Fatalf("fenced content should not be transformed, got %q", got)
	}
	if !strings.Contains(got, "<pre><code>fmt.Println(&#34;hi&#34;)</code></pre>") {
		t.Fatalf("expected code fence rendered as pre/code, got %q", got)
	}
}

func TestMarkdownToTelegramHTML_UnclosedFenceKeptRaw(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")"
	got := MarkdownToTelegramHTML(input)

	if !strings.Contains(got, "```go") {
		t.Fatalf("expected unclosed fence marker to stay raw, got %q", got)
	}
}

func TestMarkdownToTelegramHTML_NestedFormatting(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"bold code", "**`code`**", "<b><code>code</code></b>"},
		{"italic code", "*`code`*", "<i><code>code</code></i>"},
		{"bold italic code", "***`code`***", "<b><i><code>code</code></i></b>"},
		{"code in bold text", "**bold `code` here**", "<b>bold <code>code</code> here</b>"},
		{"multiple codes in bold", "**`code1` and `code2`**", "<b><code>code1</code> and <code>code2</code></b>"},
		{"link in bold", "**[link](https://example.com)**", `<b><a href="https://example.com">link</a></b>`},
		{"strikethrough code", "~~`code`~~", "<s><code>code</code></s>"},
		{"mixed formatting", "**bold *italic `code`* here**", "<b>bold <i>italic <code>code</code></i> here</b>"},
		{"code with formatting inside", "`**bold**`", "<code>**bold**</code>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MarkdownToTelegramHTML(tt.input)
			if !strings.Contains(result, tt.expected) {
				t.Errorf("MarkdownToTelegramHTML(%q) = %q, expected to contain %q", tt.input, result, tt.expected)
			}
		})
	}
}
